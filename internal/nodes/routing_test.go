package nodes_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
)

// nodeConstants maps the identifiers internal/graph exports onto their
// values, so this test can read a call written as graph.NodePlan.
var nodeConstants = map[string]string{
	"NodePlan":    graph.NodePlan,
	"NodeApprove": graph.NodeApprove,
	"NodeCode":    graph.NodeCode,
	"NodeWrite":   graph.NodeWrite,
	"NodeTest":    graph.NodeTest,
	"NodeFix":     graph.NodeFix,
	"NodeReview":  graph.NodeReview,
	"NodeFanout":  graph.NodeFanout,
	"NodeJoin":    graph.NodeJoin,
	"NodePromote": graph.NodePromote,
}

// config.RoutableNodes is the closed set agent.models may be keyed by, and it
// is only worth anything if it is the set of nodes that really do ask for a
// model. The two can drift in both directions, and each way is a quiet
// failure rather than a loud one:
//
//   - a node that starts calling ModelFor without being added here gets
//     agent.model whatever the user writes, and belay rejects the key that
//     would have routed it, insisting the node cannot take a model;
//   - a node listed here that asks for no model accepts a key, prices it in
//     the dry run, and never spends a cent on it.
//
// So this reads the calls rather than trusting a list. It scans every node
// implementation for ModelFor(graph.NodeX) and compares what it finds with
// what config allows. A slice that teaches a fourth node to call a model
// fails here until it adds the node to routableNodes.
func TestEveryNodeThatAsksForAModelIsRoutable(t *testing.T) {
	asking := map[string]string{} // node name -> the file that asks
	fset := token.NewFileSet()

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fun, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || fun.Sel.Name != "ModelFor" || len(call.Args) != 1 {
				return true
			}
			arg, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok {
				t.Errorf("%s: ModelFor is called with something other than a graph.Node* constant; "+
					"this test reads the constant to know which nodes ask for a model", fset.Position(call.Pos()))
				return true
			}
			name, known := nodeConstants[arg.Sel.Name]
			if !known {
				t.Errorf("%s: ModelFor is called with graph.%s, which this test does not know; add it to nodeConstants",
					fset.Position(call.Pos()), arg.Sel.Name)
				return true
			}
			asking[name] = fset.Position(call.Pos()).String()
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the node implementations: %v", err)
	}
	if len(asking) == 0 {
		t.Fatal("no node was found calling ModelFor; this guard would silently pass forever")
	}

	routable := config.RoutableNodes()
	for node, where := range asking {
		if !slices.Contains(routable, node) {
			t.Errorf("%s asks for a model at %s, but agent.models refuses the key %q (it allows %v)",
				node, where, node, routable)
		}
	}
	for _, node := range routable {
		if _, ok := asking[node]; !ok {
			t.Errorf("agent.models accepts the key %q, but no node calls ModelFor(graph.Node...) for %s, "+
				"so the setting would load and do nothing", node, node)
		}
	}
}
