package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"mime"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
)

// https://t3.chat/chat/15f4bccb-f941-4f6f-b996-3b8b1b4fac7f

func main() {
	// Flags
	from := flag.String("from", "", "Envelope sender (From) email address")
	to := flag.String("to", "", "Comma-separated list of To recipients")
	cc := flag.String("cc", "", "Comma-separated list of Cc recipients")
	bcc := flag.String("bcc", "", "Comma-separated list of Bcc recipients")
	subject := flag.String("subject", "", "Email subject")
	subjectFile := flag.String("subject-file", "", "Path to file containing the email subject (plain text)")
	bodyFile := flag.String("body-file", "", "Path to a file containing the email body (plain text)")
	attach := flag.String("attach", "", "Comma-separated list of file paths to attach")
	smtpHost := flag.String("smtp-host", "localhost", "SMTP host (default: localhost)")
	smtpPort := flag.String("smtp-port", "25", "SMTP port (default: 25)")
	recipientsFile := flag.String("recipients-file", "", "Path to file containing recipient lists (To, CC, Bcc)")
	logFile := flag.String("log-file", "", "Optional log file path")
	flag.Parse()

	// Setup logging
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to open log file: %v\n", err)
			os.Exit(1)
		}
		log.SetOutput(f)
	} else {
		log.SetOutput(os.Stdout)
	}
	log.Println("Starting go_sendemail program")

	// Basic validation
	if *from == "" {
		log.Fatal("Error: -from is required")
	}
	if *bodyFile == "" {
		log.Fatal("Error: -body-file is required")
	}
	// Load body from file
	body, err := loadBodyFromFile(*bodyFile)
	if err != nil {
		log.Fatalf("Error loading body from file: %v", err)
	}
	log.Println("Body loaded from file")

	// Load subject from file if provided, else use -subject
	subj := strings.TrimSpace(*subject)
	if *subjectFile != "" {
		subjFromFile, err := loadSubjectFromFile(*subjectFile)
		if err != nil {
			log.Fatalf("Error loading subject from file: %v", err)
		}
		// Prefer subject file value if non-empty
		if strings.TrimSpace(subjFromFile) != "" {
			subj = strings.TrimSpace(subjFromFile)
		}
	}
	if subj == "" {
		log.Fatal("Error: Subject is empty. Provide -subject or -subject-file with a non-empty value.")
	}
	log.Printf("Subject set: %s", subj)

	// Load recipients (flags take precedence)
	toList := splitCSV(*to)
	ccList := splitCSV(*cc)
	bccList := splitCSV(*bcc)

	// If no recipients via flags, try recipients-file
	if len(toList) == 0 && len(ccList) == 0 && len(bccList) == 0 && *recipientsFile != "" {
		toFromFile, ccFromFile, bccFromFile, err := loadRecipientsFromFile(*recipientsFile)
		if err != nil {
			log.Fatalf("Error loading recipients from file: %v", err)
		}
		toList = append(toList, toFromFile...)
		ccList = append(ccList, ccFromFile...)
		bccList = append(bccList, bccFromFile...)
		log.Println("Recipients loaded from file")
	}

	// Fallback to environment variables if still empty
	if len(toList) == 0 && len(ccList) == 0 && len(bccList) == 0 {
		envTo := os.Getenv("EMAIL_TO")
		envCc := os.Getenv("EMAIL_CC")
		envBcc := os.Getenv("EMAIL_BCC")
		toList = append(toList, splitCSV(envTo)...)
		ccList = append(ccList, splitCSV(envCc)...)
		bccList = append(bccList, splitCSV(envBcc)...)
		if len(toList) > 0 || len(ccList) > 0 || len(bccList) > 0 {
			log.Println("Recipients loaded from environment variables")
		}
	}

	// Validate recipients
	recipients := append([]string{}, toList...)
	recipients = append(recipients, ccList...)
	recipients = append(recipients, bccList...)

	// If still empty, error out
	if len(recipients) == 0 {
		log.Fatal("Error: No recipients provided via -to, -cc, -bcc, -recipients-file, or environment variables")
	}
	// Build MIME message
	msg, err := buildMessage(*from, recipients, strings.Join(toList, ", "), strings.Join(ccList, ", "), strings.Join(bccList, ", "), subj, body, *attach)
	if err != nil {
		log.Fatalf("Error building message: %v", err)
	}

	addr := fmt.Sprintf("%s:%s", *smtpHost, *smtpPort)

	// SendMail requires the envelope recipients slice; we use all recipients (To, Cc, Bcc)
	err = smtp.SendMail(addr, nil, *from, recipients, msg)
	if err != nil {
		log.Fatalf("Error sending email: %v", err)
	}
	log.Println("Email sent successfully")
}

// loadBodyFromFile reads the file and returns (content, error)
func loadBodyFromFile(path string) (string, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// loadSubjectFromFile reads and returns the subject from the given file (first line or entire content as subject)
func loadSubjectFromFile(path string) (string, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
	}
	// Use the first line as the subject, trim spaces
	lines := strings.Split(string(data), "\n")
	subj := strings.TrimSpace(lines[0])
	return subj, nil
}

// splitCSV splits a comma-separated string into a slice, trimming spaces and ignoring blanks
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadRecipientsFromFile parses a simple key=value per line recipients file.
// Example content:
// To=alice@example.com,bob@example.com
// Cc=carol@example.com
// Bcc=dave@example.com
func loadRecipientsFromFile(path string) (to []string, cc []string, bcc []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// skip empty or comment lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])
		switch key {
		case "to":
			to = append(to, splitCSV(val)...)
		case "cc":
			cc = append(cc, splitCSV(val)...)
		case "bcc":
			bcc = append(bcc, splitCSV(val)...)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, nil, err
	}
	return to, cc, bcc, nil
}

// buildMessage constructs a MIME email with optional attachments.
// - from: sender email (From)
// - recipients: all envelope recipients (To + Cc + Bcc) for header and sending
// - toHeader, ccHeader: header values for To and Cc
// - subject: email subject
// - body: plain text body
// - attachList: comma-separated file paths to attach
func buildMessage(from string, recipients []string, toHeader string, ccHeader string, bccHeader string, subject string, body string, attachList string) ([]byte, error) {
	var buf bytes.Buffer
	writer := bufio.NewWriter(&buf)

	// Basic headers
	fmt.Fprintf(writer, "From: %s\r\n", mimeHeader(from))
	fmt.Fprintf(writer, "To: %s\r\n", joinAddresses(splitCSV(toHeader)))
	// Include CC header if provided
	if strings.TrimSpace(ccHeader) != "" {
		fmt.Fprintf(writer, "Cc: %s\r\n", joinAddresses(splitCSV(ccHeader)))
	}
	// Bcc is typically not included in headers
	fmt.Fprintf(writer, "Subject: %s\r\n", mailEscape(subject))
	// MIME version and boundary
	boundary := "BOUNDARY1234567890"
	fmt.Fprintln(writer, "MIME-Version: 1.0")
	fmt.Fprintf(writer, "Content-Type: multipart/mixed; boundary=%s\r\n", boundary)
	fmt.Fprintln(writer)

	// Start multipart
	// --boundary
	fmt.Fprintf(writer, "--%s\r\n", boundary)
	// Plain text part
	fmt.Fprintln(writer, "Content-Type: text/plain; charset=utf-8")
	fmt.Fprintln(writer, "Content-Transfer-Encoding: 7bit")
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, body)
	fmt.Fprintln(writer)

	// Attachments
	attachments := splitCSV(attachList)
	for _, path := range attachments {
		if path == "" {
			continue
		}
		if err := addAttachment(writer, boundary, path); err != nil {
			return nil, err
		}
	}

	// End boundary
	fmt.Fprintf(writer, "--%s--\r\n", boundary)

	writer.Flush()
	return buf.Bytes(), nil
}

// addAttachment writes a single attachment part to the writer
func addAttachment(w *bufio.Writer, boundary string, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening attachment %s: %w", path, err)
	}
	defer file.Close()

	// Guess content type
	ctype := mime.TypeByExtension(filepath.Ext(path))
	if ctype == "" {
		ctype = "application/octet-stream"
	}

	// Read file content
	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat attachment %s: %w", path, err)
	}
	_ = stat // not used further, but kept for potential future use

	// Prepare header
	filename := filepath.Base(path)
	fmt.Fprintf(w, "--%s\r\n", boundary)
	fmt.Fprintf(w, "Content-Type: %s; name=\"%s\"\r\n", ctype, escapeQuotes(filename))
	fmt.Fprintln(w, "Content-Transfer-Encoding: base64")
	fmt.Fprintf(w, "Content-Disposition: attachment; filename=\"%s\"\r\n", escapeQuotes(filename))
	fmt.Fprintln(w)

	// Base64 encode the file
	encoder := base64.NewEncoder(base64.StdEncoding, w)
	// Use a buffered copy to limit memory usage
	bufBytes := make([]byte, 4096)
	reader := bufio.NewReader(file)
	for {
		n, err := reader.Read(bufBytes)
		if n > 0 {
			_, e2 := encoder.Write(bufBytes[:n])
			if e2 != nil {
				return e2
			}
		}
		if err != nil {
			break
		}
	}
	encoder.Close()
	fmt.Fprintln(w)
	return nil
}

// Utility helpers

func mailEscape(s string) string {
	// Very simple escape for header field content
	// In production, consider proper MIME encoding
	return s
}

func mimeHeader(s string) string {
	// Basic header escaping for an address
	return s
}

func joinAddresses(addrs []string) string {
	return strings.Join(addrs, ", ")
}

func escapeQuotes(s string) string {
	return strings.ReplaceAll(s, "\"", "\\\"")
}
