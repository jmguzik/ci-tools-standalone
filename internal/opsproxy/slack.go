package opsproxy

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const slackHTTPTimeout = 15 * time.Second

const (
	actionAck2h          = "ops-proxy:ack:2h"
	actionAck4h          = "ops-proxy:ack:4h"
	actionAck8h          = "ops-proxy:ack:8h"
	actionAck16h         = "ops-proxy:ack:16h"
	actionAck24h         = "ops-proxy:ack:24h"
	actionAck2d          = "ops-proxy:ack:2d"
	actionAckMonday      = "ops-proxy:ack:monday"
	actionUnack          = "ops-proxy:unack"
	actionNeedsHuman     = "ops-proxy:needs-human"
	boardFallback        = "CURRENT INCIDENTS"
	maxTopicChars        = 250
	maxHeaderChars       = 150
	slackActionsPerBlock = 5
)

// SlackClient is the chat.update / pin / topic surface. Tests use an interface fake.
type SlackClient interface {
	PostMessage(channel, text string, blocks []slack.Block) (channelID, ts string, err error)
	UpdateMessage(channel, ts, text string, blocks []slack.Block) error
	PinMessage(channel, ts string) error
	SetTopic(channel, topic string) error
}

type slackAPI struct {
	token      func() []byte
	httpClient *http.Client
}

func NewSlackAPI(token func() []byte) SlackClient {
	return &slackAPI{
		token:      token,
		httpClient: &http.Client{Timeout: slackHTTPTimeout},
	}
}

func (s *slackAPI) client() *slack.Client {
	var tok string
	if s.token != nil {
		tok = string(s.token())
	}
	return slack.New(tok, slack.OptionHTTPClient(s.httpClient))
}

func (s *slackAPI) PostMessage(channel, text string, blocks []slack.Block) (string, string, error) {
	ch, ts, err := s.client().PostMessage(channel, slack.MsgOptionText(text, false), slack.MsgOptionBlocks(blocks...))
	return ch, ts, err
}

func (s *slackAPI) UpdateMessage(channel, ts, text string, blocks []slack.Block) error {
	_, _, _, err := s.client().UpdateMessage(channel, ts, slack.MsgOptionText(text, false), slack.MsgOptionBlocks(blocks...))
	return err
}

func (s *slackAPI) PinMessage(channel, ts string) error {
	err := s.client().AddPin(channel, slack.NewRefToMessage(channel, ts))
	if err != nil && strings.Contains(err.Error(), "already_pinned") {
		return nil
	}
	return err
}

func (s *slackAPI) SetTopic(channel, topic string) error {
	_, err := s.client().SetTopicOfConversation(channel, topic)
	return err
}

type cardStatus string

const (
	cardOPEN       cardStatus = "OPEN"
	cardACKED      cardStatus = "ACKED"
	cardNeedsHuman cardStatus = "NEEDS HUMAN"
	cardResolved   cardStatus = "RESOLVED"
)

func statusOf(inc IncidentState, muted bool) cardStatus {
	if muted {
		return cardACKED
	}
	if inc.NeedsHuman {
		return cardNeedsHuman
	}
	return cardOPEN
}

func cardBlocks(inc IncidentState, status cardStatus, mutedUntil string, ackedBy string) []slack.Block {
	header := truncate(fmt.Sprintf("%s: %s", status, inc.Identity.ID), maxHeaderChars)
	body := fmt.Sprintf("*id:* `%s`\n*alertname:* `%s`", inc.Identity.ID, inc.Identity.AlertName)
	if inc.Identity.MatcherName != "" {
		body += fmt.Sprintf("\n*%s:* `%s`", inc.Identity.MatcherName, inc.Identity.MatcherValue)
	}
	switch status {
	case cardACKED:
		until := mutedUntil
		if until == "" {
			until = inc.EndsAt
		}
		who := ackedBy
		if who == "" {
			who = inc.AckedBy
		}
		body += fmt.Sprintf("\n*ACKED* by `%s` until `%s`", who, until)
		if inc.NeedsHuman {
			body += "\n*NEEDS HUMAN*"
		}
	case cardNeedsHuman:
		body += "\n*NEEDS HUMAN* (not muted)"
	case cardResolved:
		body += "\nResolved; dropped from the board."
	}

	blocks := []slack.Block{
		slack.NewHeaderBlock(slack.NewTextBlockObject(slack.PlainTextType, header, false, false)),
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, body, false, false), nil, nil),
	}
	if status != cardResolved {
		blocks = append(blocks, ackActionBlocks(inc.Identity.ID)...)
	}
	return blocks
}

func ackActionBlocks(incidentID string) []slack.Block {
	buttons := []*slack.ButtonBlockElement{
		ackButton(actionAck2h, "2h", incidentID, ""),
		ackButton(actionAck4h, "4h", incidentID, ""),
		ackButton(actionAck8h, "8h", incidentID, ""),
		ackButton(actionAck16h, "16h", incidentID, ""),
		ackButton(actionAck24h, "24h", incidentID, slack.StylePrimary),
		ackButton(actionAck2d, "2d", incidentID, ""),
		ackButton(actionAckMonday, "Monday", incidentID, ""),
		ackButton(actionUnack, "Unack", incidentID, slack.StyleDanger),
		ackButton(actionNeedsHuman, "Needs human", incidentID, ""),
	}
	var blocks []slack.Block
	for i := 0; i < len(buttons); i += slackActionsPerBlock {
		end := i + slackActionsPerBlock
		if end > len(buttons) {
			end = len(buttons)
		}
		chunk := buttons[i:end]
		elems := make([]slack.BlockElement, 0, len(chunk))
		for _, b := range chunk {
			elems = append(elems, b)
		}
		blocks = append(blocks, slack.NewActionBlock(fmt.Sprintf("ops-proxy-actions-%d", i/slackActionsPerBlock), elems...))
	}
	return blocks
}

func ackButton(actionID, label, incidentID string, style slack.Style) *slack.ButtonBlockElement {
	btn := slack.NewButtonBlockElement(actionID, incidentID, slack.NewTextBlockObject(slack.PlainTextType, label, true, false))
	if style != "" {
		btn.WithStyle(style)
	}
	return btn
}

func boardBlocks(incidents map[string]IncidentState, muted map[string]Silence) []slack.Block {
	var open, acked, human []string
	for id, inc := range incidents {
		name := inc.Identity.ShortName()
		if name == "" {
			name = id
		}
		if _, ok := muted[id]; ok {
			until := inc.EndsAt
			if sil := muted[id]; !sil.EndsAt.IsZero() {
				until = sil.EndsAt.UTC().Format(time.RFC3339)
			}
			acked = append(acked, fmt.Sprintf("• `%s` until %s", name, until))
			continue
		}
		if inc.NeedsHuman {
			human = append(human, "• `"+name+"`")
			continue
		}
		open = append(open, "• `"+name+"`")
	}
	sort.Strings(open)
	sort.Strings(acked)
	sort.Strings(human)
	text := "*CURRENT INCIDENTS*\n"
	text += fmt.Sprintf("*OPEN (%d)*\n", len(open))
	if len(open) == 0 {
		text += "_none_\n"
	} else {
		text += strings.Join(open, "\n") + "\n"
	}
	if len(human) > 0 {
		text += fmt.Sprintf("*NEEDS HUMAN (%d)*\n%s\n", len(human), strings.Join(human, "\n"))
	}
	if len(acked) > 0 {
		text += fmt.Sprintf("*ACKED (%d)*\n%s\n", len(acked), strings.Join(acked, "\n"))
	}
	return []slack.Block{
		slack.NewHeaderBlock(slack.NewTextBlockObject(slack.PlainTextType, "CURRENT INCIDENTS", false, false)),
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, strings.TrimSpace(text), false, false), nil, nil),
	}
}

func FormatTopic(openNames []string) string {
	n := len(openNames)
	if n == 0 {
		return "RED 0 OPEN"
	}
	prefix := fmt.Sprintf("RED %d OPEN · ", n)
	names := strings.Join(openNames, ", ")
	s := prefix + names
	if len(s) <= maxTopicChars {
		return s
	}
	budget := maxTopicChars - len(prefix)
	const ellipsis = "…"
	if budget <= len(ellipsis) {
		return truncate(prefix, maxTopicChars)
	}
	return prefix + names[:budget-len(ellipsis)] + ellipsis
}

func openNames(incidents map[string]IncidentState, muted map[string]Silence) []string {
	var names []string
	for id, inc := range incidents {
		if _, ok := muted[id]; ok {
			continue
		}
		name := inc.Identity.ShortName()
		if name == "" {
			name = id
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	if n == 1 {
		return s[:1]
	}
	return s[:n-1] + "…"
}
