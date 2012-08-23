package txn

import (
	"fmt"
	"labix.org/v2/mgo"
	"labix.org/v2/mgo/bson"
	"sort"
)

func flush(r *Runner, t *transaction) error {
	f := &flusher{
		Runner:   r,
		goal:     t,
		goalKeys: make(map[docKey]bool),
		queue:    make(map[docKey][]token),
		debugId:  debugPrefix(),
	}
	for _, dkey := range f.goal.docKeys() {
		f.goalKeys[dkey] = true
	}
	return f.run()
}

type flusher struct {
	*Runner
	goal     *transaction
	goalKeys map[docKey]bool
	queue    map[docKey][]token
	debugId  string
}

func (f *flusher) run() (err error) {
	if chaosEnabled {
		defer f.handleChaos(&err)
	}

	f.debugf("Processing %s", f.goal)
	seen := make(map[bson.ObjectId]*transaction)
	if err := f.recurse(f.goal, seen); err != nil {
		return err
	}
	if f.goal.done() {
		return nil
	}

	// Sparse workloads will generally be managed entirely by recurse.
	// Getting here means one or more transactions have dependencies
	// and perhaps cycles.

	// Build successors data for Tarjan's sort. Must consider
	// that entries in txn-queue are not necessarily valid.
	successors := make(map[bson.ObjectId][]bson.ObjectId)
	ready := true
	for _, dqueue := range f.queue {
	NextPair:
		for i := 0; i < len(dqueue); i++ {
			pred := dqueue[i]
			predid := pred.id()
			predt := seen[predid]
			if predt == nil || predt.Nonce != pred.nonce() {
				continue
			}
			predsuccids, ok := successors[predid]
			if !ok {
				successors[predid] = nil
			}

			for j := i + 1; j < len(dqueue); j++ {
				succ := dqueue[j]
				succid := succ.id()
				succt := seen[succid]
				if succt == nil || succt.Nonce != succ.nonce() {
					continue
				}
				if _, ok := successors[succid]; !ok {
					successors[succid] = nil
				}

				// Found a valid pred/succ pair.
				i = j - 1
				for _, predsuccid := range predsuccids {
					if predsuccid == succid {
						continue NextPair
					}
				}
				successors[predid] = append(predsuccids, succid)
				if succid == f.goal.Id {
					// There are still pre-requisites to handle.
					ready = false
				}
				continue NextPair
			}
		}
	}
	f.debugf("Queues: %v", f.queue)
	f.debugf("Successors: %v", successors)
	if ready {
		f.debugf("Goal %s has no real pre-requisites", f.goal)
		return f.advance(f.goal, nil, true)
	}

	// Robert Tarjan's algorithm for detecting strongly-connected
	// components is used for topological sorting and detecting
	// cycles at once. The order in which transactions are applied
	// in commonly affected documents must be a global agreement.
	sorted := tarjanSort(successors)
	if debugEnabled {
		f.debugf("Tarjan output: %v", sorted)
	}
	pull := make(map[bson.ObjectId]*transaction)
	for i := len(sorted) - 1; i >= 0; i-- {
		scc := sorted[i]
		f.debugf("Flushing %v", scc)
		if len(scc) == 1 {
			pull[scc[0]] = seen[scc[0]]
		}
		for _, id := range scc {
			if err := f.advance(seen[id], pull, true); err != nil {
				return err
			}
		}
		if len(scc) > 1 {
			for _, id := range scc {
				pull[id] = seen[id]
			}
		}
	}
	return nil
}

func (f *flusher) recurse(t *transaction, seen map[bson.ObjectId]*transaction) error {
	seen[t.Id] = t
	err := f.advance(t, nil, false)
	if err != errPreReqs {
		return err
	}
	for _, dkey := range t.docKeys() {
		for _, dtt := range f.queue[dkey] {
			id := dtt.id()
			if seen[id] != nil {
				continue
			}
			qt, err := f.load(id)
			if err != nil {
				return err
			}
			err = f.recurse(qt, seen)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *flusher) advance(t *transaction, pull map[bson.ObjectId]*transaction, force bool) error {
	for {
		switch t.State {
		case tpreparing, tprepared:
			revnos, err := f.prepare(t, force)
			if err != nil {
				return err
			}
			if t.State != tprepared {
				continue
			}
			if err = f.assert(t, revnos, pull); err != nil {
				return err
			}
			if t.State != tprepared {
				continue
			}
			if err = f.checkpoint(t, revnos); err != nil {
				return err
			}
		case tapplying:
			return f.apply(t, pull)
		case taborting:
			return f.abortOrReload(t, nil, pull)
		case tapplied, taborted:
			return nil
		default:
			panic(fmt.Errorf("transaction in unknown state: %q", t.State))
		}
	}
	panic("unreachable")
}

type stash string

const (
	stashStable stash = ""
	stashInsert stash = "insert"
	stashRemove stash = "remove"
)

type txnInfo struct {
	Queue []token `bson:"txn-queue"`
	Revno int64   `bson:"txn-revno,omitempty"`
	Insert bson.ObjectId `bson:"txn-insert,omitempty"`
	Remove bson.ObjectId `bson:"txn-remove,omitempty"`
}

type stashState string

const (
	stashNew stashState = ""
	stashInserting stashState = "inserting"

)

var txnFields = bson.D{{"txn-queue", 1}, {"txn-revno", 1}, {"txn-remove", 1}, {"txn-insert", 1}}

var errPreReqs = fmt.Errorf("transaction has pre-requisites and force is false")

// prepare injects t's id onto txn-queue for all affected documents
// and collects the current txn-queue and txn-revno values during
// the process. If the prepared txn-queue indicates that there are
// pre-requisite transactions to be applied and the force parameter
// is false, errPreReqs will be returned. Otherwise, the current
// tip revision numbers for all the documents are returned.
func (f *flusher) prepare(t *transaction, force bool) (revnos []int64, err error) {
	if t.State != tpreparing {
		return f.rescan(t, force)
	}
	f.debugf("Preparing %s", t)

	// Iterate in a stable way across all runners. This isn't
	// strictly required, but reduces the chances of cycles.
	dkeys := t.docKeys()
	sort.Sort(dkeys)

	revno := make(map[docKey]int64)
	info := txnInfo{}
	tt := tokenFor(t)
NextDoc:
	for _, dkey := range dkeys {
		change := mgo.Change{
			Update:    bson.D{{"$addToSet", bson.D{{"txn-queue", tt}}}},
			ReturnNew: true,
		}
		c := f.tc.Database.C(dkey.Collection)
		cquery := c.FindId(dkey.DocId).Select(txnFields)

RetryDoc:
		change.Upsert = false
		chaos("")
		if _, err := cquery.Apply(change, &info); err == nil {
			if info.Remove == "" {
				// Fast path, unless workload is insert/remove heavy.
				revno[dkey] = info.Revno
				f.queue[dkey] = info.Queue
				f.debugf("[A] Prepared document %v with revno %d and queue: %v", dkey, info.Revno, info.Queue)
				continue NextDoc
			} else {
				// Handle remove in progress before preparing it.
				if err := f.loadAndApply(info.Remove); err != nil {
					return nil, err
				}
				goto RetryDoc
			}
		} else if err != mgo.ErrNotFound {
			return nil, err
		}

		// Document missing. Use stash collection.
		change.Upsert = true
		chaos("")
		_, err := f.sc.FindId(dkey).Apply(change, &info)
		if err != nil {
			return nil, err
		}
		if info.Insert != "" {
			// Handle insert in progress before preparing it.
			if err := f.loadAndApply(info.Insert); err != nil {
				return nil, err
			}
			goto RetryDoc
		}

		// Must confirm stash is still in use and is the same one
		// prepared, since applying a remove overwrites the stash.
		docFound := false
		stashFound := false
		if err = c.FindId(dkey.DocId).Select(txnFields).One(&info); err == nil {
			docFound = true
		} else if err != mgo.ErrNotFound {
			return nil, err
		} else if err = f.sc.FindId(dkey).One(&info); err == nil {
			stashFound = true
			if info.Revno == 0 {
				// Missing revno in the stash only happens when it
				// has been upserted, in which case it defaults to -1.
				info.Revno = -1
			}
		} else if err != mgo.ErrNotFound {
			return nil, err
		}

		if docFound && info.Remove == "" || stashFound && info.Insert == "" {
			for _, dtt := range info.Queue {
				if dtt != tt {
					continue
				}
				// Found tt properly prepared.
				if stashFound {
					f.debugf("[B] Prepared document %v on stash with revno %d and queue: %v", dkey, info.Revno, info.Queue)
				} else {
					f.debugf("[B] Prepared document %v with revno %d and queue: %v", dkey, info.Revno, info.Queue)
				}
				revno[dkey] = info.Revno
				f.queue[dkey] = info.Queue
				continue NextDoc
			}
		}

		// The stash wasn't valid and tt got overwriten. Try again.
		f.unstashToken(tt, dkey)
		goto RetryDoc
	}

	// Save the prepared nonce onto t.
	nonce := tt.nonce()
	qdoc := bson.D{{"_id", t.Id}, {"s", tpreparing}}
	udoc := bson.D{{"$set", bson.D{{"s", tprepared}, {"n", nonce}}}}
	chaos("")
	err = f.tc.Update(qdoc, udoc)
	if err == nil {
		t.State = tprepared
		t.Nonce = nonce
	} else if err == mgo.ErrNotFound {
		f.debugf("Can't save nonce of %s: LOST RACE", tt)
		if err := f.reload(t); err != nil {
			return nil, err
		} else if t.State == tpreparing {
			panic("can't save nonce yet transaction is still preparing")
		} else if t.State != tprepared {
			return t.Revnos, nil
		}
		tt = t.token()
	} else if err != nil {
		return nil, err
	}

	prereqs, found := f.hasPreReqs(tt, dkeys)
	if !found {
		// Must only happen when reloading above.
		return f.rescan(t, force)
	} else if prereqs && !force {
		f.debugf("Prepared queue with %s [has prereqs & not forced].", tt)
		return nil, errPreReqs
	}
	for _, op := range t.Ops {
		dkey := op.docKey()
		revnos = append(revnos, revno[dkey])
		if op.isChange() {
			revno[dkey] += 1
		}
	}
	if !prereqs {
		f.debugf("Prepared queue with %s [no prereqs]. Revnos: %v", tt, revnos)
	} else {
		f.debugf("Prepared queue with %s [forced] Revnos: %v", tt, revnos)
	}
	return revnos, nil
}

func (f *flusher) unstashToken(tt token, dkey docKey) error {
	qdoc := bson.D{{"_id", dkey}, {"txn-queue", tt}}
	udoc := bson.D{{"$pull", bson.D{{"txn-queue", tt}}}}
	chaos("")
	if err := f.sc.Update(qdoc, udoc); err == nil {
		chaos("")
		err = f.sc.Remove(bson.D{{"_id", dkey}, {"txn-queue", bson.D{}}})
	} else if err != mgo.ErrNotFound {
		return err
	}
	return nil
}

func (f *flusher) rescan(t *transaction, force bool) (revnos []int64, err error) {
	f.debugf("Rescanning %s", t)
	if t.State != tprepared {
		panic(fmt.Errorf("rescanning transaction in invalid state: %q", t.State))
	}

	// Iterate in a stable way across all runners. This isn't
	// strictly required, but reduces the chances of cycles.
	dkeys := t.docKeys()
	sort.Sort(dkeys)

	tt := t.token()
	if !force {
		prereqs, found := f.hasPreReqs(tt, dkeys)
		if found && prereqs {
			// It's state is already known.
			return nil, errPreReqs
		}
	}

	revno := make(map[docKey]int64)
	info := txnInfo{}
	for _, dkey := range dkeys {
		retry := 0

RetryDoc:
		c := f.tc.Database.C(dkey.Collection)
		if err := c.FindId(dkey.DocId).Select(txnFields).One(&info); err == mgo.ErrNotFound {
			// Document is missing. Look in stash.
			if err := f.sc.FindId(dkey).One(&info); err == mgo.ErrNotFound {
				// Stash also doesn't exist. Maybe someone applied it.
				if err := f.reload(t); err != nil {
					return nil, err
				} else if t.State != tprepared {
					return t.Revnos, err
				}
				// Not applying either.
				retry++
				if retry < 3 {
					// Retry since there might be an insert/remove race.
					goto RetryDoc
				}
				// Neither the doc nor the stash seem to exist.
				return nil, fmt.Errorf("cannot find document %v for applying transaction %s", dkey, t)
			} else if err != nil {
				return nil, err
			}
			// Stash found.
			if info.Insert != "" {
				// Handle insert in progress before assuming ordering is good.
				if err := f.loadAndApply(info.Insert); err != nil {
					return nil, err
				}
				goto RetryDoc
			}
			if info.Revno == 0 {
				// Missing revno in the stash means -1.
				info.Revno = -1
			}
		} else if err != nil {
			return nil, err
		} else if info.Remove != "" {
			// Handle remove in progress before assuming ordering is good.
			if err := f.loadAndApply(info.Remove); err != nil {
				return nil, err
			}
			goto RetryDoc
		}
		revno[dkey] = info.Revno

		found := false
		for _, id := range info.Queue {
			if id == tt {
				found = true
				break
			}
		}
		f.queue[dkey] = info.Queue
		if !found {
			// Previously set txn-queue was popped by someone.
			// Transaction is being/has been applied elsewhere.
			f.debugf("Rescanned document %v misses %s in queue: %v", dkey, tt, info.Queue)
			err := f.reload(t)
			if t.State == tpreparing || t.State == tprepared {
				panic("rescanned document misses transaction in queue")
			}
			return t.Revnos, err
		}
	}

	prereqs, found := f.hasPreReqs(tt, dkeys)
	if !found {
		panic("rescanning loop guarantees that this can't happen")
	} else if prereqs && !force {
		f.debugf("Rescanned queue with %s: has prereqs, not forced", tt)
		return nil, errPreReqs
	}
	for _, op := range t.Ops {
		dkey := op.docKey()
		revnos = append(revnos, revno[dkey])
		if op.isChange() {
			revno[dkey] += 1
		}
	}
	if !prereqs {
		f.debugf("Rescanned queue with %s: no prereqs, revnos: %v", tt, revnos)
	} else {
		f.debugf("Rescanned queue with %s: has prereqs, forced, revnos: %v", tt, revnos)
	}
	return revnos, nil
}

func (f *flusher) hasPreReqs(tt token, dkeys docKeys) (prereqs, found bool) {
	found = true
NextDoc:
	for _, dkey := range dkeys {
		for _, dtt := range f.queue[dkey] {
			if dtt == tt {
				continue NextDoc
			} else if dtt.id() != tt.id() {
				prereqs = true
			}
		}
		found = false
	}
	return
}

func (f *flusher) reload(t *transaction) error {
	var newt transaction
	query := f.tc.FindId(t.Id)
	query.Select(bson.D{{"s", 1}, {"n", 1}, {"r", 1}})
	if err := query.One(&newt); err != nil {
		return fmt.Errorf("failed to reload transaction: %v", err)
	}
	t.State = newt.State
	t.Nonce = newt.Nonce
	t.Revnos = newt.Revnos
	f.debugf("Reloaded %s: %q", t, t.State)
	return nil
}

func (f *flusher) loadAndApply(id bson.ObjectId) error {
	t, err := f.load(id)
	if err != nil {
		return err
	}
	return f.advance(t, nil, true)
}

// assert verifies that all assertions in t match the content that t
// will be applied upon. If an assertion fails, the transaction state
// is changed to aborted.
func (f *flusher) assert(t *transaction, revnos []int64, pull map[bson.ObjectId]*transaction) error {
	f.debugf("Asserting %s with revnos %v", t, revnos)
	if t.State != tprepared {
		panic(fmt.Errorf("asserting transaction in invalid state: %q", t.State))
	}
	qdoc := make(bson.D, 3)
	revno := make(map[docKey]int64)
	for i, op := range t.Ops {
		dkey := op.docKey()
		if _, ok := revno[dkey]; !ok {
			revno[dkey] = revnos[i]
		}
		if op.Assert == nil {
			continue
		}
		if op.Assert == DocMissing {
			if revnos[i] >= 0 {
				return f.abortOrReload(t, revnos, pull)
			}
			continue
		}
		if op.Insert != nil {
			return fmt.Errorf("Insert can only Assert txn.DocMissing", op.Assert)
		}
		// if revnos[i] < 0 { abort }?

		qdoc = append(qdoc[:0], bson.DocElem{"_id", op.DocId})
		if op.Assert != DocMissing {
			var revnoq interface{}
			if n := revno[dkey]; n == 0 {
				revnoq = bson.D{{"$exists", false}}
			} else {
				revnoq = n
			}
			// XXX Add tt to the query here, once we're sure it's all working.
			//     Not having it increases the chances of breaking on bad logic.
			qdoc = append(qdoc, bson.DocElem{"txn-revno", revnoq})
			if op.Assert != DocExists {
				qdoc = append(qdoc, bson.DocElem{"$or", []interface{}{op.Assert}})
			}
		}

		c := f.tc.Database.C(op.Collection)
		if err := c.Find(qdoc).Select(bson.D{{"_id", 1}}).One(nil); err == mgo.ErrNotFound {
			// Assertion failed or someone else started applying.
			return f.abortOrReload(t, revnos, pull)
		} else if err != nil {
			return err
		}
	}
	f.debugf("Asserting %s succeeded", t)
	return nil
}

func (f *flusher) abortOrReload(t *transaction, revnos []int64, pull map[bson.ObjectId]*transaction) (err error) {
	f.debugf("Aborting or reloading %s (was %q)", t, t.State)
	if t.State == tprepared {
		qdoc := bson.D{{"_id", t.Id}, {"s", tprepared}}
		udoc := bson.D{{"$set", bson.D{{"s", taborting}}}}
		chaos("")
		if err = f.tc.Update(qdoc, udoc); err == nil {
			t.State = taborting
		} else if err == mgo.ErrNotFound {
			if err = f.reload(t); err != nil || t.State != taborting {
				f.debugf("Won't abort %s. Reloaded state: %q", t, t.State)
				return err
			}
		} else {
			return err
		}
	} else if t.State != taborting {
		panic(fmt.Errorf("aborting transaction in invalid state: %q", t.State))
	}

	if len(revnos) > 0 {
		if pull == nil {
			pull = map[bson.ObjectId]*transaction{t.Id: t}
		}
		seen := make(map[docKey]bool)
		for i, op := range t.Ops {
			dkey := op.docKey()
			if seen[op.docKey()] {
				continue
			}
			seen[dkey] = true

			pullAll := tokensToPull(f.queue[dkey], pull, "")
			if len(pullAll) == 0 {
				continue
			}
			udoc := bson.D{{"$pullAll", bson.D{{"txn-queue", pullAll}}}}
			chaos("")
			if revnos[i] < 0 {
				err = f.sc.UpdateId(dkey, udoc)
			} else {
				c := f.tc.Database.C(dkey.Collection)
				err = c.UpdateId(dkey.DocId, udoc)
			}
			if err != nil && err != mgo.ErrNotFound {
				return err
			}
		}
	}
	udoc := bson.D{{"$set", bson.D{{"s", taborted}}}}
	chaos("")
	if err := f.tc.UpdateId(t.Id, udoc); err != nil && err != mgo.ErrNotFound {
		return err
	}
	t.State = taborted
	f.debugf("Aborted %s", t)
	return nil
}

func (f *flusher) checkpoint(t *transaction, revnos []int64) error {
	var debugRevnos map[docKey][]int64
	if debugEnabled {
		debugRevnos = make(map[docKey][]int64)
		for i, op := range t.Ops {
			dkey := op.docKey()
			debugRevnos[dkey] = append(debugRevnos[dkey], revnos[i])
		}
		f.debugf("Ready to apply %s. Saving revnos %v", t, debugRevnos)
	}

	// Save in t the txn-revno values the transaction must run on.
	qdoc := bson.D{{"_id", t.Id}, {"s", tprepared}}
	udoc := bson.D{{"$set", bson.D{{"s", tapplying}, {"r", revnos}}}}
	chaos("checkpoint")
	err := f.tc.Update(qdoc, udoc)
	if err == nil {
		t.State = tapplying
		t.Revnos = revnos
		f.debugf("Ready to apply %s. Saving revnos %v: DONE", t, debugRevnos)
	} else if err == mgo.ErrNotFound {
		f.debugf("Ready to apply %s. Saving revnos %v: LOST RACE", t, debugRevnos)
		return f.reload(t)
	}
	return nil
}

func (f *flusher) apply(t *transaction, pull map[bson.ObjectId]*transaction) error {
	f.debugf("Applying transaction %s", t)
	if t.State != tapplying {
		panic(fmt.Errorf("applying transaction in invalid state: %q", t.State))
	}
	if pull == nil {
		pull = map[bson.ObjectId]*transaction{t.Id: t}
	}

	// Compute the last change for each doc. That's when
	// t's id may be pulled out of txn-queue.
	lastChange := make(map[docKey]int)
	for i, op := range t.Ops {
		dkey := op.docKey()
		if _, ok := lastChange[dkey]; !ok || op.isChange() {
			lastChange[dkey] = i
		}
	}

	tt := tokenFor(t)
	for i, op := range t.Ops {
		dkey := op.docKey()
		dqueue := f.queue[dkey]
		revno := t.Revnos[i]

		var opName string
		if debugEnabled {
			opName = op.name()
			f.debugf("Applying %s op %d (%s) on %v with txn-revno %d", t, i, opName, dkey, revno)
		}

		c := f.tc.Database.C(op.Collection)
		var revnoq, idq interface{}
		if revno == 0 || op.Insert != nil && revno == -1 {
			revnoq = bson.D{{"$exists", false}}
		} else {
			revnoq = revno
		}
		if op.Insert != nil {
			idq = dkey
		} else {
			idq = dkey.DocId
		}
		qdoc := bson.D{{"_id", idq}, {"txn-revno", revnoq}, {"txn-queue", tt}}

		dontPull := tt
		if lastChange[dkey] == i {
			dontPull = ""
		}
		pullAll := tokensToPull(dqueue, pull, dontPull)

		var m map[string]interface{}
		var outcome string
		var err error
		switch {
		case op.Update != nil:
			if revno < 0 {
				err = mgo.ErrNotFound
			} else {
				if m, err = objToMap(op.Update); err != nil {
					return err
				}
				addToMap(m, "$pullAll", bson.D{{"txn-queue", pullAll}})
				addToMap(m, "$inc", bson.D{{"txn-revno", 1}})
				chaos("")
				err = c.Update(qdoc, m)
			}
		case op.Remove:
			if revno < 0 {
				err = mgo.ErrNotFound
			} else {
				nonce := newNonce()
				stash := txnInfo{}
				change := mgo.Change{
					Update: bson.D{{"$push", bson.D{{"n", nonce}}}},
					Upsert: true,
					ReturnNew: true,
				}
				if _, err = f.sc.FindId(dkey).Apply(change, &stash); err != nil {
					return err
				}
				change = mgo.Change{
					Update: bson.D{{"$set", bson.D{{"txn-remove", t.Id}}}},
					ReturnNew: true,
				}
				var info txnInfo
				if _, err = c.Find(qdoc).Apply(change, &info); err == nil {
					// The document still exists so the stash previously
					// observed was either out of date or necessarily
					// contained the token being applied.
					f.debugf("Marked document %v to be removed on revno %d with queue: %v", dkey, info.Revno, info.Queue)
					updated := false
					if !hasToken(stash.Queue, tt) {
						var set, unset bson.D
						if revno == 0 {
							// Missing revno in stash means -1.
							set = bson.D{{"txn-queue", info.Queue}}
							unset = bson.D{{"n", 1}, {"txn-revno", 1}}
						} else {
							set = bson.D{{"txn-queue", info.Queue}, {"txn-revno", -revno-1}}
							unset = bson.D{{"n", 1}}
						}
						qdoc := bson.D{{"_id", dkey}, {"n", nonce}}
						udoc := bson.D{{"$set", set}, {"$unset", unset}}
						if err = f.sc.Update(qdoc, udoc); err == nil {
							updated = true
						} else if err != mgo.ErrNotFound {
							return err
						}
					}
					if updated {
						f.debugf("Updated stash for document %v with revno %d and queue: %v", dkey, -revno-1, info.Queue)
					} else {
						f.debugf("Stash for document %v was up-to-date", dkey)
					}
					err = c.Remove(qdoc)
				}
			}
		case op.Insert != nil:
			if revno >= 0 {
				err = mgo.ErrNotFound
			} else {
				if m, err = objToMap(op.Insert); err != nil {
					return err
				}
				change := mgo.Change{
					Update: bson.D{{"$set", bson.D{{"txn-insert", t.Id}}}},
					ReturnNew: true,
				}
				chaos("")
				var info txnInfo
				if _, err = f.sc.Find(qdoc).Apply(change, &info); err == nil {
					f.debugf("Stash for document %v has revno %d and queue: %v", dkey, info.Revno, info.Queue)
					m["_id"] = op.DocId
					m["txn-revno"] = -revno+1
					m["txn-queue"] = info.Queue
					// Unlikely yet unfortunate race in here if this gets seriously
					// delayed. If someone inserts+removes meanwhile, this will
					// reinsert, and there's no way to avoid that while keeping the
					// collection clean or compromising sharding. applyOps can solve
					// the former, but it can't shard (SERVER-1439).
					chaos("insert")
					err = c.Insert(m)
					if lerr, ok := err.(*mgo.LastError); err == nil || ok && lerr.Code == 11000 {
						if err == nil {
							f.debugf("New document %v inserted with revno %d and queue: %v", dkey, info.Revno, info.Queue)
						} else {
							f.debugf("Document %v already existed", dkey)
						}
						chaos("")
						if err = f.sc.Remove(qdoc); err == nil {
							f.debugf("Stash for document %v removed", dkey)
						}
					}
					if lastChange[dkey] == i && len(pullAll) > 0 {
						_ = f.sc.UpdateId(dkey, bson.D{{"$pullAll", bson.D{{"txn-queue", pullAll}}}})
					}
				}
			}
		}
		if err == nil {
			outcome = "DONE"
		} else if lerr, ok := err.(*mgo.LastError); ok && lerr.Code == 11000 || err == mgo.ErrNotFound {
			outcome = "MISS"
			err = nil
		} else {
			outcome = err.Error()
		}
		if debugEnabled {
			f.debugf("Applying %s op %d (%s) on %v with txn-revno %d: %s", t, i, opName, dkey, revno, outcome)
		}
		if err != nil {
			return err
		}
	}
	t.State = tapplied

	// It's been applied, so errors are ignored here. It's fine for someone
	// else to win the race and mark it as applied, and it's also fine for
	// it to remain pending until a later point when someone will perceive
	// it has been applied and mark it at such.
	chaos("")
	f.debugf("Marking %s as applied", t)
	f.tc.Update(bson.D{{"_id", t.Id}, {"s", tapplying}}, bson.D{{"$set", bson.D{{"s", tapplied}}}})
	return nil
}

func tokensToPull(dqueue []token, pull map[bson.ObjectId]*transaction, dontPull token) []token {
	var result []token
	for j := len(dqueue) - 1; j >= 0; j-- {
		dtt := dqueue[j]
		if dt, ok := pull[dtt.id()]; ok {
			if dt.Nonce == dtt.nonce() {
				// It's valid and is being pulled out, so everything
				// preceding it must have been handled already.
				if dtt == dontPull {
					// Not time to pull this one out yet.
					j--
				}
				result = append(result, dqueue[:j+1]...)
				break
			}
			// It was handled before and this is a leftover invalid
			// nonce in the queue. Cherry-pick it out.
			result = append(result, dtt)
		}
	}
	return result
}

func objToMap(obj interface{}) (m map[string]interface{}, err error) {
	data, err := bson.Marshal(obj)
	if err != nil {
		return nil, err
	}
	err = bson.Unmarshal(data, &m)
	if err != nil {
		return nil, err
	}
	return m, err
}

func addToMap(m map[string]interface{}, key string, add bson.D) error {
	if old, ok := m[key]; ok {
		if oldm, ok := old.(bson.M); ok {
			for _, elem := range add {
				oldm[elem.Name] = elem.Value
			}
		} else {
			return fmt.Errorf("invalid %q value in change document: %#v", key, old)
		}
	} else {
		m[key] = add
	}
	return nil
}

func hasToken(tokens []token, tt token) bool {
	for _, ttt := range tokens {
		if ttt == tt {
			return true
		}
	}
	return false
}

func (f *flusher) debugf(format string, args ...interface{}) {
	if !debugEnabled {
		return
	}
	debugf(f.debugId+format, args...)
}
