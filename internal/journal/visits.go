package journal

// Visit is one visit the run paid to a node: every execution of the node
// from the one that began the visit to the last one that took it up again.
//
// A node's executions and its visits are different counts, and a person
// means visits. Records count executions: the approve gate pausing for a
// person and running again when they resume the run is two executions, and
// so is an attempt a crash or a Ctrl-C cut short and the re-run that
// followed. Neither is the graph coming back to the node. Numbering them as
// two would tell a person the run looped where it waited, and would move
// every later number of that node — the number they read in `belay timeline`
// and then type into `belay fork --visit`.
type Visit struct {
	// Node is the node visited.
	Node string
	// Ordinal is which visit to Node this is, 1-based, in the order the run
	// reached them: test's first visit is 1, and its visit after the first
	// repair is 2.
	Ordinal int
	// StartSeq is the Seq of the node_started that began the visit. It names
	// the visit: it is unique within the run, it is durable before the node
	// runs, and it does not change however many times the visit is taken up
	// again. The blackboard snapshot taken before the visit is keyed on it.
	StartSeq uint64
	// EndSeq is the Seq of the node_finished that closed the visit's last
	// execution, or 0 while that execution has not finished.
	EndSeq uint64
	// Executions is how many times the node ran within the visit: 1, unless
	// the visit was taken up again after a pause or an interruption.
	Executions int
	// Status is the outcome of the visit's last execution, or StatusUnknown
	// while that execution has not finished. A visit whose last record hands
	// it back to the node (see continuesVisit) is not over: the run takes it
	// up again when it next runs.
	Status Status
}

// Visits groups records' node executions into visits, in the order the run
// began them. records are a journal in file order, as Read returns them.
//
// An execution continues the visit before it exactly when the record that
// closed the previous execution hands the visit back to the same node: see
// continuesVisit. That is the one rule. The dispatcher's snapshots and a
// fork's --visit are chosen by it, and `belay timeline` numbers its rows by
// it, so the three cannot disagree about which visit is which.
//
// Visits validates records first (see Validate) and returns the error it
// reports, with no visits. It does not degrade the way Stats does: a history
// whose executions do not pair up has no single right grouping, and a fork
// that guessed would re-run a node from a blackboard belonging to a
// different visit. A caller that must show something for a damaged journal,
// as the timeline does, decides for itself what to show without numbers.
func Visits(records []Record) ([]Visit, error) {
	if err := Validate(records); err != nil {
		return nil, err
	}

	var (
		visits   []Visit
		ordinals = make(map[string]int)
		// closed is the node_finished that closed the previous execution.
		// Validate has established that every node_started follows a closed
		// execution, so when one is seen, closed is the record that decides
		// whether it continues a visit.
		closed *Record
	)
	for i := range records {
		rec := records[i]
		switch rec.Event {
		case EventNodeStarted:
			if closed != nil && continuesVisit(*closed, rec.Node) {
				v := &visits[len(visits)-1]
				v.Executions++
				v.EndSeq, v.Status = 0, StatusUnknown
				continue
			}
			ordinals[rec.Node]++
			visits = append(visits, Visit{
				Node: rec.Node, Ordinal: ordinals[rec.Node], StartSeq: rec.Seq, Executions: 1,
			})
		case EventNodeFinished:
			v := &visits[len(visits)-1]
			v.EndSeq, v.Status = rec.Seq, rec.Status
			closed = &rec
		default:
			// Run-level markers — a pause, a resume — sit between executions
			// and belong to no visit. They do not break one either: a gate's
			// pause is followed by run_paused and run_resumed before the gate
			// runs again, and it is still one visit.
		}
	}
	return visits, nil
}

// continuesVisit reports whether an execution of node that follows closed —
// the node_finished of the execution before it — takes up that execution's
// visit rather than beginning a new one.
//
// A node_finished that names its own node as what comes next has not
// finished the visit. With StatusPaused it is a gate that stopped for a
// person; with StatusAborted it is an attempt a crash or a Ctrl-C cut short
// (Record.Interrupted). Either way the execution that follows is the same
// visit taken up again: the graph did not route anywhere and come back.
//
// A node that routes to itself with StatusOK is not this. That is the graph
// deciding to go round again, and it is a new visit.
//
// This is the rule `belay timeline` first used to stop reporting the approval
// gate as a loop (commit 8f46bac). It lives here so there is one copy of it.
func continuesVisit(closed Record, node string) bool {
	if closed.Node != node || closed.Next != node {
		return false
	}
	return closed.Status == StatusPaused || closed.Status == StatusAborted
}
