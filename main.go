// go-sendemail sends one e-mail (plain-text body, optional attachments) through an SMTP server.
// Built for scripts and cron jobs: one static binary, standard library only, no config file.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var version = "dev" // set at build time: -ldflags "-X main.version=v1.0.0"

// Message is everything needed to build and send one e-mail.
type Message struct {
	From        *mail.Address
	To, Cc, Bcc []*mail.Address
	Subject     string
	Body        string
	Attachments []string // file paths
}

func main() {
	log.SetFlags(log.LstdFlags)
	log.SetOutput(os.Stderr)
	if err := run(os.Args[1:]); err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("go-sendemail", flag.ContinueOnError)
	from := fs.String("from", "", "sender address, e.g. 'Backups <backup@example.com>' (required)")
	to := fs.String("to", "", "comma-separated To recipients")
	cc := fs.String("cc", "", "comma-separated Cc recipients")
	bcc := fs.String("bcc", "", "comma-separated Bcc recipients (envelope only, never in the headers)")
	subject := fs.String("subject", "", "subject")
	subjectFile := fs.String("subject-file", "", "file whose first line is the subject (wins over -subject)")
	bodyFile := fs.String("body-file", "", "file with the plain-text body; '-' reads standard input (required)")
	attach := fs.String("attach", "", "comma-separated files to attach")
	recipientsFile := fs.String("recipients-file", "", "file with To=/Cc=/Bcc= lines, used when no recipient flag is given")
	smtpHost := fs.String("smtp-host", "localhost", "SMTP server")
	smtpPort := fs.String("smtp-port", "25", "SMTP port")
	tlsMode := fs.String("tls", "auto", "auto = STARTTLS when the server offers it, certificate verified; off = never")
	logFile := fs.String("log-file", "", "append log lines to this file instead of standard error")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("opening log file: %w", err)
		}
		defer f.Close()
		log.SetOutput(f)
	}
	if *tlsMode != "auto" && *tlsMode != "off" {
		return fmt.Errorf("-tls must be auto or off, not %q", *tlsMode)
	}

	msg := &Message{}
	var err error
	if *from == "" {
		return errors.New("-from is required")
	}
	if msg.From, err = parseAddress(*from); err != nil {
		return fmt.Errorf("-from: %w", err)
	}

	if *bodyFile == "" {
		return errors.New("-body-file is required ('-' for standard input)")
	}
	if msg.Body, err = readBody(*bodyFile); err != nil {
		return fmt.Errorf("reading body: %w", err)
	}

	msg.Subject = strings.TrimSpace(*subject)
	if *subjectFile != "" {
		s, err := firstLine(*subjectFile)
		if err != nil {
			return fmt.Errorf("reading subject file: %w", err)
		}
		if s != "" {
			msg.Subject = s
		}
	}
	if msg.Subject == "" {
		return errors.New("subject is empty: give -subject or a non-empty -subject-file")
	}

	// Recipients: flags, else the recipients file, else EMAIL_TO / EMAIL_CC / EMAIL_BCC.
	toS, ccS, bccS := splitCSV(*to), splitCSV(*cc), splitCSV(*bcc)
	if len(toS)+len(ccS)+len(bccS) == 0 && *recipientsFile != "" {
		if toS, ccS, bccS, err = loadRecipientsFile(*recipientsFile); err != nil {
			return fmt.Errorf("reading recipients file: %w", err)
		}
	}
	if len(toS)+len(ccS)+len(bccS) == 0 {
		toS, ccS, bccS = splitCSV(os.Getenv("EMAIL_TO")), splitCSV(os.Getenv("EMAIL_CC")), splitCSV(os.Getenv("EMAIL_BCC"))
	}
	for _, l := range []struct {
		name string
		in   []string
		out  *[]*mail.Address
	}{{"to", toS, &msg.To}, {"cc", ccS, &msg.Cc}, {"bcc", bccS, &msg.Bcc}} {
		for _, s := range l.in {
			a, err := parseAddress(s)
			if err != nil {
				return fmt.Errorf("%s recipient %q: %w", l.name, s, err)
			}
			*l.out = append(*l.out, a)
		}
	}
	if len(msg.To)+len(msg.Cc)+len(msg.Bcc) == 0 {
		return errors.New("no recipients: use -to/-cc/-bcc, -recipients-file or EMAIL_TO/EMAIL_CC/EMAIL_BCC")
	}
	msg.Attachments = splitCSV(*attach)

	data, err := msg.Build(time.Now())
	if err != nil {
		return fmt.Errorf("building message: %w", err)
	}
	addr := net.JoinHostPort(*smtpHost, *smtpPort)
	if err := send(addr, *smtpHost, *tlsMode == "auto", msg.From.Address, msg.Envelope(), data); err != nil {
		return fmt.Errorf("sending via %s: %w", addr, err)
	}
	log.Printf("sent %q to %d recipient(s) via %s", msg.Subject, len(msg.Envelope()), addr)
	return nil
}

// Envelope returns every recipient address (To, Cc and Bcc) for RCPT TO.
func (m *Message) Envelope() []string {
	var out []string
	for _, l := range [][]*mail.Address{m.To, m.Cc, m.Bcc} {
		for _, a := range l {
			out = append(out, a.Address)
		}
	}
	return out
}

// Build returns the message in wire format: CRLF line endings, MIME multipart/mixed, the body quoted-printable,
// attachments base64 in 76-character lines, a random boundary, Date and Message-ID headers. Bcc is not written.
func (m *Message) Build(now time.Time) ([]byte, error) {
	if strings.ContainsAny(m.Subject, "\r\n") {
		return nil, errors.New("subject contains a line break")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := func(k, v string) { fmt.Fprintf(&buf, "%s: %s\r\n", k, v) }
	hdr("From", m.From.String())
	if len(m.To) > 0 {
		hdr("To", joinAddresses(m.To))
	}
	if len(m.Cc) > 0 {
		hdr("Cc", joinAddresses(m.Cc))
	}
	hdr("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("Message-ID", messageID(m.From.Address))
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "multipart/mixed;\r\n\tboundary="+mw.Boundary()) // folded: the line stays under 78 characters
	buf.WriteString("\r\n")

	part, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=utf-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return nil, err
	}
	qp := quotedprintable.NewWriter(part)
	if _, err := io.WriteString(qp, strings.ReplaceAll(m.Body, "\r\n", "\n")); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}

	for _, path := range m.Attachments {
		if err := addAttachment(mw, path); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func addAttachment(mw *multipart.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("attachment: %w", err)
	}
	defer f.Close()
	name := filepath.Base(path)
	ctype := mime.TypeByExtension(filepath.Ext(path))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	part, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {mime.FormatMediaType(ctype, map[string]string{"name": name})},
		"Content-Transfer-Encoding": {"base64"},
		"Content-Disposition":       {mime.FormatMediaType("attachment", map[string]string{"filename": name})},
	})
	if err != nil {
		return err
	}
	lw := &lineWrapper{w: part, max: 76}
	enc := base64.NewEncoder(base64.StdEncoding, lw)
	if _, err := io.Copy(enc, f); err != nil {
		return fmt.Errorf("attachment %s: %w", path, err)
	}
	if err := enc.Close(); err != nil {
		return err
	}
	_, err = io.WriteString(part, "\r\n")
	return err
}

// lineWrapper inserts CRLF every max bytes (RFC 2045: base64 lines of at most 76 characters).
type lineWrapper struct {
	w   io.Writer
	max int
	n   int
}

func (l *lineWrapper) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := l.max - l.n
		if chunk > len(p) {
			chunk = len(p)
		}
		n, err := l.w.Write(p[:chunk])
		written += n
		if err != nil {
			return written, err
		}
		l.n += n
		p = p[chunk:]
		if l.n == l.max {
			if _, err := io.WriteString(l.w, "\r\n"); err != nil {
				return written, err
			}
			l.n = 0
		}
	}
	return written, nil
}

// send delivers data over SMTP. With tryTLS, STARTTLS is used when the server offers it (certificate checked
// against host); for a local relay with a self-signed certificate, use -tls off.
func send(addr, host string, tryTLS bool, from string, rcpts []string, data []byte) error {
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Hello(localName()); err != nil {
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); ok && tryTLS {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("STARTTLS (use -tls off for a local relay with a self-signed certificate): %w", err)
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("recipient %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func localName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "localhost"
}

func messageID(from string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	domain := "localhost"
	if i := strings.LastIndexByte(from, '@'); i >= 0 && i < len(from)-1 {
		domain = from[i+1:]
	}
	return fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), hex.EncodeToString(b), domain)
}

func parseAddress(s string) (*mail.Address, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, errors.New("address contains a line break")
	}
	return mail.ParseAddress(s)
}

func joinAddresses(l []*mail.Address) string {
	s := make([]string, len(l))
	for i, a := range l {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

func readBody(path string) (string, error) {
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	return string(b), err
}

func firstLine(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line), nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadRecipientsFile reads To=, Cc= and Bcc= lines (comma-separated values; # starts a comment).
func loadRecipientsFile(path string) (to, cc, bcc []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "to":
			to = append(to, splitCSV(v)...)
		case "cc":
			cc = append(cc, splitCSV(v)...)
		case "bcc":
			bcc = append(bcc, splitCSV(v)...)
		}
	}
	return to, cc, bcc, sc.Err()
}
