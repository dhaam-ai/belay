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

// Model is the viewer's state. It is a bubbletea model: every change to it
// arrives as a message through Update, which is what lets its tests drive it
// with key presses and no terminal at all.
type Model struct{}

var _ tea.Model = Model{}

// Init implements tea.Model.
func (Model) Init() tea.Cmd { return nil }

// Update implements tea.Model.
func (m Model) Update(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }

// View implements tea.Model.
func (Model) View() tea.View { return tea.NewView("") }
