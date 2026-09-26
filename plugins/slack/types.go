package slack

import (
	"strings"

	slackgo "github.com/slack-go/slack"
)

// Message is what the transport renders and the client posts.
//
// The blocks are the library's, so a card is built from the same types Slack
// documents rather than from a copy of them that can drift. The text is always
// set as well: it is what a notification shows and what a client that cannot
// render blocks falls back to.
type Message struct {
	Text   string
	Blocks []slackgo.Block
}

// downloadURL is where a file's bytes are.
//
// Slack requires the bot token to read it, so it is not a public link.
func downloadURL(f slackgo.File) string {
	if f.URLPrivateDownload != "" {
		return f.URLPrivateDownload
	}
	return f.URLPrivate
}

// isImage reports whether a file is a picture.
func isImage(f slackgo.File) bool {
	return strings.HasPrefix(f.Mimetype, "image/")
}
