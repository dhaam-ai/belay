// Package planview is belay's plan viewer: a full-screen terminal page where
// a person reads the plan a run stopped to show them, edits its steps, and
// then approves it, asks the agent for a different one, or leaves the run
// paused.
//
// It is the only package in belay that imports a terminal library
// (boundary_test.go enforces it), and only the command layer imports it.
package planview

import (
	tea "charm.land/bubbletea/v2"
)

// Options is what the viewer is given to show.
type Options struct{}

// Action is what the person decided to do with the run.
type Action int

// The decisions a person can leave the viewer with.
const (
	// ActionLeave leaves the run as it is: paused at its plan.
	ActionLeave Action = iota
)

// Decision is what the person decided, which the command layer carries out
// once the viewer has closed.
type Decision struct {
	// Action is what they decided.
	Action Action
	// Guidance is what they said a new plan should do differently.
	Guidance string
}

// Model is the viewer's state. It is a bubbletea model: every change to it
// arrives as a message through Update, which is what lets its tests drive it
// with key presses and no terminal at all.
type Model struct {
	decision Decision
}

var _ tea.Model = Model{}

// New returns a viewer showing what opts describes.
func New(Options) Model { return Model{} }

// Decision returns what the person decided. It is ActionLeave until they
// decide something else.
func (m Model) Decision() Decision { return m.decision }

// Init implements tea.Model.
func (Model) Init() tea.Cmd { return nil }

// Update implements tea.Model.
func (m Model) Update(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }

// View implements tea.Model.
func (Model) View() tea.View { return tea.NewView("") }
