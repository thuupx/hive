package slack

import (
	"testing"

	slackgo "github.com/slack-go/slack"
)

// The renderer builds the library's blocks, so the tests read them back the same
// way a reader of the Slack documentation would.

// sectionText is the text of a section block.
func sectionText(t *testing.T, block slackgo.Block) string {
	t.Helper()

	section, ok := block.(*slackgo.SectionBlock)
	if !ok || section.Text == nil {
		t.Fatalf("block is not a section with text: %T", block)
	}
	return section.Text.Text
}

// actionsBlock is the actions block of a message.
func actionsBlock(t *testing.T, message Message) *slackgo.ActionBlock {
	t.Helper()

	for _, block := range message.Blocks {
		if actions, ok := block.(*slackgo.ActionBlock); ok {
			return actions
		}
	}
	t.Fatal("the message has no actions block")
	return nil
}

// buttons are the buttons of an actions block.
func buttons(t *testing.T, actions *slackgo.ActionBlock) []*slackgo.ButtonBlockElement {
	t.Helper()

	if actions.Elements == nil {
		t.Fatal("the actions block has no elements")
	}
	out := make([]*slackgo.ButtonBlockElement, 0, len(actions.Elements.ElementSet))
	for _, element := range actions.Elements.ElementSet {
		button, ok := element.(*slackgo.ButtonBlockElement)
		if !ok {
			t.Fatalf("element is not a button: %T", element)
		}
		out = append(out, button)
	}
	return out
}

// elementsOf are the interactive elements of a block.
//
// Slack checks action ids across the elements of one block, so a test that guards
// uniqueness reads them the same way.
func elementsOf(block slackgo.Block) []slackgo.BlockElement {
	if actions, ok := block.(*slackgo.ActionBlock); ok && actions.Elements != nil {
		return actions.Elements.ElementSet
	}
	return nil
}
