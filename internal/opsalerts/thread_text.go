package opsalerts

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// threadReferenceDirectory resolves already known Slack handles without network reads.
type threadReferenceDirectory interface {
	ResolveThreadReference(string, string) (string, bool)
}

// ThreadReferenceError keeps unresolved mentions from silently becoming ordinary text.
type ThreadReferenceError struct{ Reference string }

// Error tells the operator how to preserve an actionable or deliberately literal reference.
func (e *ThreadReferenceError) Error() string {
	reference := e.Reference
	if len(reference) > 128 {
		reference = reference[:128] + "…"
	}
	return fmt.Sprintf("cannot resolve Slack reference %q; use a known handle or a Slack reference such as <@USERID> or <#CHANNELID>, or quote it as code for literal text", reference)
}

// Unwrap makes an unresolved reference a definite rejection before any Slack write.
func (*ThreadReferenceError) Unwrap() error { return ErrValidation }

// attributedThreadReplyHeader records the authenticated operator in the visible Slack message.
func attributedThreadReplyHeader(actor string) string {
	return threadReplyPrefix + escapeSlack(actor) + "):\n\n"
}

// threadReplyDisplayBody accepts legacy framing and Slack-normalized visible attribution.
// The trusted metadata records the submitting actor; the editable display header does not.
func threadReplyDisplayBody(text string) (string, bool) {
	if body, found := strings.CutPrefix(text, threadReplyHeader); found {
		return body, true
	}
	rest, found := strings.CutPrefix(text, threadReplyPrefix)
	if !found {
		return "", false
	}
	header, body, found := strings.Cut(rest, "):\n\n")
	if !found || header == "" || strings.ContainsAny(header, "\r\n") {
		return "", false
	}
	return body, true
}

// threadReplyBody preserves explicit Slack references while escaping unrelated angle brackets.
// Only known handles outside code and URLs are promoted to notifying/linking references.
func (s *Service) threadReplyBody(text, channelName string) (string, error) {
	var body strings.Builder
	for i := 0; i < len(text); {
		if text[i] == '`' {
			delimiter := "`"
			if strings.HasPrefix(text[i:], "```") {
				delimiter = "```"
			}
			end := len(text)
			if close := strings.Index(text[i+len(delimiter):], delimiter); close >= 0 {
				end = i + len(delimiter) + close + len(delimiter)
			}
			body.WriteString(escapeSlack(text[i:end]))
			i = end
			continue
		}
		if strings.HasPrefix(text[i:], "https://") || strings.HasPrefix(text[i:], "http://") {
			end := i
			for end < len(text) {
				r, size := utf8.DecodeRuneInString(text[end:])
				if unicode.IsSpace(r) {
					break
				}
				end += size
			}
			body.WriteString(escapeSlack(text[i:end]))
			i = end
			continue
		}
		if text[i] == '<' && (i == 0 || text[i-1] != '\\') {
			end := strings.IndexByte(text[i:], '>')
			if end >= 0 && !strings.ContainsAny(text[i:i+end], "\r\n") {
				token := text[i+1 : i+end]
				if strings.HasPrefix(token, "@") || strings.HasPrefix(token, "#") {
					id, _, _ := strings.Cut(token[1:], "|")
					if !validThreadReference(token[:1], id) {
						return "", &ThreadReferenceError{Reference: text[i : i+end+1]}
					}
					body.WriteString("<" + token[:1] + id + ">")
				} else if token == "!here" || token == "!channel" || token == "!everyone" || strings.HasPrefix(token, "!subteam^") {
					return "", &ThreadReferenceError{Reference: text[i : i+end+1]}
				} else {
					body.WriteString(escapeSlack(text[i : i+end+1]))
				}
				i += end + 1
				continue
			}
			if strings.HasPrefix(text[i:], "<@") || strings.HasPrefix(text[i:], "<#") {
				return "", &ThreadReferenceError{Reference: text[i:]}
			}
		}
		if (text[i] == '@' || text[i] == '#') && threadReferenceBoundary(text[:i]) {
			end := i + 1
			for end < len(text) && threadHandleByte(text[end]) {
				end++
			}
			name := text[i+1 : end]
			if end < len(text) {
				r, size := utf8.DecodeRuneInString(text[end:])
				if r >= utf8.RuneSelf && (name == "" && !unicode.IsSpace(r) || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r)) {
					return "", &ThreadReferenceError{Reference: text[i : end+size]}
				}
			}
			if name != "" && (text[i] != '#' || !allThreadDigits(name)) {
				kind := text[i : i+1]
				id, found := s.resolveThreadReference(kind, name, channelName)
				if !found && strings.HasSuffix(name, ".") {
					trimmed := strings.TrimRight(name, ".")
					id, found = s.resolveThreadReference(kind, trimmed, channelName)
					if found {
						end -= len(name) - len(trimmed)
					}
				}
				if !found {
					return "", &ThreadReferenceError{Reference: text[i:end]}
				}
				body.WriteString("<" + kind + id + ">")
				i = end
				continue
			}
		}
		switch text[i] {
		case '&':
			body.WriteString("&amp;")
		case '<':
			body.WriteString("&lt;")
		case '>':
			body.WriteString("&gt;")
		default:
			body.WriteByte(text[i])
		}
		i++
	}
	return body.String(), nil
}

// resolveThreadReference keeps configured Chai mentions explicit and otherwise uses cached identities.
func (s *Service) resolveThreadReference(kind, name, channelName string) (string, bool) {
	if kind == "#" && strings.EqualFold(name, strings.TrimPrefix(strings.TrimSpace(channelName), "#")) && validThreadReference(kind, s.cfg.ChannelID) {
		return s.cfg.ChannelID, true
	}
	if kind == "@" && (strings.EqualFold(name, "here") || strings.EqualFold(name, "channel") || strings.EqualFold(name, "everyone")) {
		return "", false
	}
	if kind == "@" && strings.EqualFold(name, "chai-bot") && validThreadReference(kind, s.chaiUser()) {
		return s.chaiUser(), true
	}
	if directory, ok := s.archive.(threadReferenceDirectory); ok {
		id, found := directory.ResolveThreadReference(kind, name)
		return id, found && validThreadReference(kind, id)
	}
	return "", false
}

// validThreadReference permits user IDs and channel IDs, never executable Slack special commands.
func validThreadReference(kind, id string) bool {
	pattern := `^[UW][A-Za-z0-9_]{1,127}$`
	if kind == "#" {
		pattern = `^[CG][A-Za-z0-9_]{1,127}$`
	}
	return regexp.MustCompile(pattern).MatchString(id)
}

// threadReferenceBoundary avoids treating email addresses and URL fragments as mentions.
func threadReferenceBoundary(prefix string) bool {
	if prefix == "" {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(prefix)
	return unicode.IsSpace(r) || strings.ContainsRune("([{,:;", r)
}

// threadHandleByte recognizes Slack's conventional ASCII handles and channel names.
func threadHandleByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("_.-", rune(b))
}

// allThreadDigits leaves issue references such as #123 as ordinary text.
func allThreadDigits(value string) bool {
	for _, b := range value {
		if b < '0' || b > '9' {
			return false
		}
	}
	return value != ""
}
