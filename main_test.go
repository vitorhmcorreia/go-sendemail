package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func addr(t *testing.T, s string) *mail.Address {
	t.Helper()
	a, err := mail.ParseAddress(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestBuildFormat(t *testing.T) {
	dir := t.TempDir()
	att := filepath.Join(dir, "big.bin")
	payload := bytes.Repeat([]byte{0, 1, 2, 250, 251, 252}, 2000) // 12 kB: one line would break the 998-char SMTP limit
	if err := os.WriteFile(att, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Message{
		From:        addr(t, "Backups <backup@example.com>"),
		To:          []*mail.Address{addr(t, "alice@example.com")},
		Cc:          []*mail.Address{addr(t, "carol@example.com")},
		Bcc:         []*mail.Address{addr(t, "dave@example.com")},
		Subject:     "Relatório diário ✓",
		Body:        "line one\nlinha dois: ação\n" + strings.Repeat("x", 200) + "\n",
		Attachments: []string{att},
	}
	raw, err := m.Build(time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	for i, line := range bytes.Split(raw, []byte("\r\n")) {
		if bytes.ContainsAny(line, "\r\n") {
			t.Fatalf("line %d has a bare CR or LF", i)
		}
		if len(line) > 78 {
			t.Fatalf("line %d is %d characters (want <= 78)", i, len(line))
		}
	}
	if bytes.Contains(raw, []byte("dave@example.com")) {
		t.Fatal("Bcc recipient appears in the message")
	}

	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"Date", "Message-Id", "From", "To", "Cc"} {
		if msg.Header.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	subj, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subj != m.Subject {
		t.Fatalf("subject = %q, %v", subj, err)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])

	p, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(quotedprintable.NewReader(p))
	if got := strings.ReplaceAll(string(body), "\r\n", "\n"); got != m.Body {
		t.Fatalf("body = %q", got)
	}

	p, err = mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if p.FileName() != "big.bin" {
		t.Fatalf("attachment name = %q", p.FileName())
	}
	got, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, p))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("attachment differs after decoding (err %v)", err)
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Fatalf("expected end of parts, got %v", err)
	}
}

func TestHeaderInjectionRejected(t *testing.T) {
	m := &Message{From: addr(t, "a@example.com"), To: []*mail.Address{addr(t, "b@example.com")},
		Subject: "hi\r\nBcc: evil@example.com", Body: "x"}
	if _, err := m.Build(time.Now()); err == nil {
		t.Fatal("subject with CRLF was accepted")
	}
	if _, err := parseAddress("a@example.com\r\nBcc: evil@example.com"); err == nil {
		t.Fatal("address with CRLF was accepted")
	}
}

func TestRecipientsFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.txt")
	os.WriteFile(p, []byte("# team\nTo=a@example.com, b@example.com\ncc=c@example.com\nBCC=d@example.com\nbogus\n"), 0o600)
	to, cc, bcc, err := loadRecipientsFile(p)
	if err != nil || len(to) != 2 || len(cc) != 1 || len(bcc) != 1 {
		t.Fatalf("to=%v cc=%v bcc=%v err=%v", to, cc, bcc, err)
	}
}

// fakeSMTP accepts one message and records the envelope and data.
type fakeSMTP struct {
	from  string
	rcpts []string
	data  string
}

func startFakeSMTP(t *testing.T, starttls bool) (string, *fakeSMTP, chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rec, done := &fakeSMTP{}, make(chan struct{})
	go func() {
		defer close(done)
		defer ln.Close()
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r, w := bufio.NewReader(c), bufio.NewWriter(c)
		say := func(s string) { w.WriteString(s + "\r\n"); w.Flush() }
		say("220 fake ESMTP")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.TrimRight(line, "\r\n")
			up := strings.ToUpper(cmd)
			switch {
			case strings.HasPrefix(up, "EHLO"):
				if starttls {
					say("250-fake\r\n250 STARTTLS")
				} else {
					say("250 fake")
				}
			case strings.HasPrefix(up, "MAIL FROM:"):
				rec.from = strings.Trim(cmd[10:], "<> ")
				say("250 ok")
			case strings.HasPrefix(up, "RCPT TO:"):
				rec.rcpts = append(rec.rcpts, strings.Trim(cmd[8:], "<> "))
				say("250 ok")
			case up == "DATA":
				say("354 go")
				var b strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				rec.data = b.String()
				say("250 queued")
			case up == "QUIT":
				say("221 bye")
				return
			default:
				say("502 no")
			}
		}
	}()
	return ln.Addr().String(), rec, done
}

func TestRunSendsToFakeServer(t *testing.T) {
	a, rec, done := startFakeSMTP(t, false)
	host, port, _ := net.SplitHostPort(a)
	dir := t.TempDir()
	body := filepath.Join(dir, "body.txt")
	os.WriteFile(body, []byte("disk at 91%\n"), 0o600)
	err := run([]string{"-from", "cron@example.com", "-to", "ops@example.com", "-bcc", "audit@example.com",
		"-subject", "disk alert", "-body-file", body, "-smtp-host", host, "-smtp-port", port})
	if err != nil {
		t.Fatal(err)
	}
	<-done
	if rec.from != "cron@example.com" || strings.Join(rec.rcpts, ",") != "ops@example.com,audit@example.com" {
		t.Fatalf("envelope from=%q rcpts=%v", rec.from, rec.rcpts)
	}
	if !strings.Contains(rec.data, "Subject: disk alert\r\n") || strings.Contains(rec.data, "audit@example.com") {
		t.Fatal("unexpected message data")
	}
}

func TestTLSOffSkipsStartTLS(t *testing.T) {
	// The fake server offers STARTTLS but can't do it: with -tls off the send must still work.
	a, _, done := startFakeSMTP(t, true)
	host, port, _ := net.SplitHostPort(a)
	body := filepath.Join(t.TempDir(), "b.txt")
	os.WriteFile(body, []byte("x"), 0o600)
	if err := run([]string{"-from", "a@example.com", "-to", "b@example.com", "-subject", "s", "-body-file", body,
		"-smtp-host", host, "-smtp-port", port, "-tls", "off"}); err != nil {
		t.Fatal(err)
	}
	<-done
}

func TestMissingArguments(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"-from", "a@example.com"},
		{"-from", "not an address", "-body-file", "/dev/null", "-subject", "s", "-to", "b@example.com"},
		{"-from", "a@example.com", "-body-file", "/dev/null", "-to", "b@example.com"},
		{"-from", "a@example.com", "-body-file", "/dev/null", "-subject", "s", "-tls", "maybe", "-to", "b@example.com"},
	} {
		t.Setenv("EMAIL_TO", "")
		if err := run(args); err == nil {
			t.Errorf("run(%q) succeeded, want an error", args)
		}
	}
}
