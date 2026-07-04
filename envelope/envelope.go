// Package envelope carries the one canonical value messenger moves between channels.
//
// messenger is broker-free: an inbound message from any channel is normalized to an
// Envelope, appended to the inbox and/or POSTed to a subscriber webhook; an outbound
// send is shaped as an Envelope and delivered by the matching channel adapter. The
// Envelope is self-describing — Channel + ThreadID name where a reply lands, ID is the
// stable per-message identity, and ReplyTo names the specific message a send answers,
// so threaded replies need no external lookup.
package envelope

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Envelope is the ONE value type messenger moves. Immutable by convention: readers
// take context off the value and never mutate a shared place.
//
//	ID        stable per-message identity (telegram message_id, wacli id, or minted)
//	Channel   originating channel: "telegram", "whatsapp", "hook"
//	Account   platform account / workspace the message arrived on
//	Sender    the human/agent identity that sent it
//	Text      the message body
//	Origin    the producer that minted it ("Telegram", "WhatsApp", "Hook")
//	ThreadID  conversation/chat the reply belongs on (telegram chat id, wa chat jid)
//	ReplyTo   the message ID this send is a reply to ("" = not a reply / new message)
//	TS        unix millis the envelope was minted
//	Meta      free-form producer annotations (never a secret value)
type Envelope struct {
	ID       string            `json:"id"`
	Channel  string            `json:"channel"`
	Account  string            `json:"account,omitempty"`
	Sender   string            `json:"sender,omitempty"`
	Text     string            `json:"text"`
	Origin   string            `json:"origin,omitempty"`
	ThreadID string            `json:"thread_id,omitempty"`
	ReplyTo  string            `json:"reply_to,omitempty"`
	TS       int64             `json:"ts,omitempty"`
	Meta     map[string]string `json:"meta,omitempty"`
}

// newID mints a random 128-bit hex id. It never encodes a secret.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Normalize fills the defaults every producer relies on: a fresh ID when absent and a
// TS when absent. Idempotent; Envelope is passed and returned by value.
func Normalize(e Envelope) Envelope {
	if e.ID == "" {
		e.ID = newID()
	}
	if e.TS == 0 {
		e.TS = time.Now().UnixMilli()
	}
	return e
}

// Inbound builds a normalized inbound Envelope from an adapter's decoded fields.
func Inbound(channel, sender, text, origin string) Envelope {
	return Normalize(Envelope{
		Channel: channel,
		Sender:  sender,
		Text:    text,
		Origin:  origin,
	})
}

// Reply builds the outbound Envelope that answers in, threading off in.ID: it lands on
// the same Channel/ThreadID and sets ReplyTo to the message being answered, so the
// adapter can quote/reply-to without any external "who asked" lookup.
func Reply(in Envelope, text string) Envelope {
	return Normalize(Envelope{
		Channel:  in.Channel,
		Account:  in.Account,
		ThreadID: in.ThreadID,
		ReplyTo:  in.ID,
		Text:     text,
		Origin:   "messenger",
	})
}
