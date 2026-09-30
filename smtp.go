package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

const (
	MaxEmailSize = 10 * 1024 * 1024 // 10MB
)

// EmailAttachment represents an inline or file attachment
type EmailAttachment struct {
	Filename  string `json:"filename"`
	Content   string `json:"content"`               // base64 encoded
	MimeType  string `json:"type"`                  // e.g., "image/png", "image/svg+xml"
	ContentID string `json:"content_id,omitempty"` // for inline images (cid: references)
}

// EmailRequest represents the request to the relay email API
type EmailRequest struct {
	To          string            `json:"to"`
	Subject     string            `json:"subject"`
	Text        string            `json:"text"`
	HTML        string            `json:"html,omitempty"`
	AppName     string            `json:"appName"`
	Attachments []EmailAttachment `json:"attachments,omitempty"`
}

// SMTPBackend implements SMTP server backend
type SMTPBackend struct {
	relayCredential  string
	relayEndpointURL string
	stats            *Store
	client           *http.Client
}

// SMTPSession represents an SMTP session
type SMTPSession struct {
	backend  *SMTPBackend
	appName  string
	from     string
	to       []string
	authUser string
}

// NewSMTPBackend creates a new SMTP backend. stats may be nil.
func NewSMTPBackend(relayEndpointURL, relayCredential string, stats *Store) *SMTPBackend {
	return &SMTPBackend{
		relayEndpointURL: relayEndpointURL,
		relayCredential:  relayCredential,
		stats:            stats,
		client:           &http.Client{Timeout: 30 * time.Second},
	}
}

// NewSession creates a new SMTP session
func (b *SMTPBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &SMTPSession{
		backend: b,
	}, nil
}

// AuthMechanisms returns supported authentication mechanisms
func (s *SMTPSession) AuthMechanisms() []string {
	return []string{sasl.Plain}
}

// Auth creates a SASL server for authentication
// NOTE: Authentication is relaxed since this runs in a private Docker network
// Apps can connect without credentials since network isolation provides security
func (s *SMTPSession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		// Accept any credentials - network isolation is the security boundary
		log.Printf("SMTP connection from app: %s", username)
		s.authUser = username
		s.appName = sanitizeAppName(username)
		return nil
	}), nil
}

// Mail sets the sender
func (s *SMTPSession) Mail(from string, opts *smtp.MailOptions) error {
	s.from = from
	return nil
}

// Rcpt adds a recipient
func (s *SMTPSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	s.to = append(s.to, to)
	return nil
}

// Data handles the email data and forwards it to the relay API.
//
// The relay API takes one recipient per call, so every envelope recipient
// (To, Cc and Bcc alike — SMTP does not distinguish them) gets its own copy,
// addressed to them alone. If any copy fails the whole transaction is reported
// as failed, so an app that retries may deliver a second copy to the
// recipients that did succeed. Status codes:
//   - 250 when every copy was accepted (including "skipped" by a relay with no
//     delivery provider — the relay said OK, the stats record the difference);
//   - 451 (temporary) when a copy was rate-limited or the relay was unreachable;
//   - 554 (permanent) when the relay refused a copy.
func (s *SMTPSession) Data(r io.Reader) error {
	// Read email data
	data, err := io.ReadAll(io.LimitReader(r, MaxEmailSize))
	if err != nil {
		return err
	}

	recipients := dedupeRecipients(s.to)
	if len(recipients) == 0 {
		return fmt.Errorf("no recipients specified")
	}

	// Parse email (now includes attachments)
	subject, text, html, attachments := parseEmail(string(data))

	// Determine app name from sender address, fall back to auth username
	var appName string
	if s.from != "" {
		parts := strings.Split(s.from, "@")
		if len(parts) > 0 {
			appName = sanitizeAppName(parts[0])
		}
	}
	if appName == "" {
		appName = s.appName
	}
	if appName == "" {
		appName = "app"
	}

	log.Printf("Processing email from app '%s' to %d recipient(s) (attachments: %d)", appName, len(recipients), len(attachments))

	var temporary, permanent []string
	for _, to := range recipients {
		res, err := s.backend.forwardToAPI(EmailRequest{
			To:          to,
			Subject:     subject,
			Text:        text,
			HTML:        html,
			AppName:     appName,
			Attachments: attachments,
		})
		ev := Event{App: appName, From: res.From, To: to, Status: StatusSent}
		switch {
		case err == nil && res.Skipped:
			ev.Status = StatusSkipped
			log.Printf("Relay accepted email for %s but skipped delivery (no delivery provider configured)", to)
		case err == nil:
			log.Printf("Email forwarded successfully from %s to %s", appName, to)
		default:
			ev.Error = err.Error()
			var re *RelayError
			if errors.As(err, &re) && re.StatusCode == http.StatusTooManyRequests {
				ev.Status = StatusRateLimited
				temporary = append(temporary, to)
			} else if errors.As(err, &re) {
				ev.Status = StatusFailed
				permanent = append(permanent, to)
			} else {
				// No HTTP answer at all: the relay is unreachable, worth a retry.
				ev.Status = StatusFailed
				temporary = append(temporary, to)
			}
			log.Printf("Failed to forward email for %s to API: %v", to, err)
		}
		s.backend.stats.Record(ev)
	}

	switch {
	case len(permanent) > 0:
		return &smtp.SMTPError{
			Code:         554,
			EnhancedCode: smtp.EnhancedCode{5, 0, 0},
			Message:      fmt.Sprintf("relay refused %d of %d recipient(s)", len(permanent), len(recipients)),
		}
	case len(temporary) > 0:
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 7, 0},
			Message:      fmt.Sprintf("relay rate limit reached or relay unreachable for %d of %d recipient(s), try again later", len(temporary), len(recipients)),
		}
	}
	return nil
}

// dedupeRecipients strips angle brackets and drops repeats (case-insensitive).
func dedupeRecipients(to []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range to {
		r = strings.TrimSpace(strings.Trim(strings.TrimSpace(r), "<>"))
		k := strings.ToLower(r)
		if r == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	return out
}

// Reset resets the session
func (s *SMTPSession) Reset() {
	s.from = ""
	s.to = nil
}

// Logout ends the session
func (s *SMTPSession) Logout() error {
	return nil
}

// RelayError is a non-2xx answer from the relay API.
type RelayError struct {
	StatusCode int
	Body       string
}

func (e *RelayError) Error() string {
	return fmt.Sprintf("API returned error status %d: %s", e.StatusCode, strings.TrimSpace(e.Body))
}

// relayResult is the relay's success body: {success, from, skipped}. From is
// the sender the relay enforced; Skipped means it accepted the email but has no
// delivery provider configured, so nothing was sent.
type relayResult struct {
	Success bool   `json:"success"`
	From    string `json:"from"`
	Skipped bool   `json:"skipped"`
}

// forwardToAPI sends one email to the relay email API. A non-2xx answer is a
// *RelayError; any other error means the relay never answered.
func (b *SMTPBackend) forwardToAPI(emailReq EmailRequest) (relayResult, error) {
	var res relayResult

	jsonData, err := json.Marshal(emailReq)
	if err != nil {
		return res, fmt.Errorf("failed to marshal email request: %w", err)
	}

	url := fmt.Sprintf("%s/email/send", b.relayEndpointURL)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return res, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", b.relayCredential))

	resp, err := b.client.Do(req)
	if err != nil {
		return res, fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return res, &RelayError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	// An older relay may answer with no body or a different one: that is still
	// a success, just without the enforced sender or the skipped flag.
	_ = json.Unmarshal(body, &res)
	return res, nil
}

// sanitizeAppName sanitizes the app name
func sanitizeAppName(name string) string {
	// Only allow alphanumeric and hyphens, lowercase, max 20 chars
	reg := regexp.MustCompile("[^a-z0-9-]")
	name = strings.ToLower(name)
	name = reg.ReplaceAllString(name, "")
	if len(name) > 20 {
		name = name[:20]
	}
	if name == "" {
		name = "app"
	}
	return name
}

// parseEmail extracts subject, text, HTML, and attachments from email data using go-message library
func parseEmail(data string) (subject, text, html string, attachments []EmailAttachment) {
	reader := strings.NewReader(data)
	entity, err := message.Read(reader)
	if err != nil {
		log.Printf("Failed to parse email: %v", err)
		return "No Subject", data, "", nil
	}

	header := entity.Header
	if subjectHeader := header.Get("Subject"); subjectHeader != "" {
		subject = subjectHeader
	} else {
		subject = "No Subject"
	}

	text, html, attachments = extractBodyParts(entity)

	if text == "" && html != "" {
		text = html
	}

	return subject, strings.TrimSpace(text), strings.TrimSpace(html), attachments
}

// extractBodyParts recursively extracts text, HTML, and attachments from a MIME message
func extractBodyParts(entity *message.Entity) (text, html string, attachments []EmailAttachment) {
	mediaType, params, err := entity.Header.ContentType()
	if err != nil {
		body, _ := io.ReadAll(entity.Body)
		return string(body), "", nil
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		mr := entity.MultipartReader()
		if mr == nil {
			return "", "", nil
		}

		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Printf("Error reading multipart: %v", err)
				break
			}

			partMediaType, _, _ := part.Header.ContentType()
			contentID := part.Header.Get("Content-Id")
			contentDisposition := part.Header.Get("Content-Disposition")

			// Extract inline images as attachments (preserve Content-ID for SendGrid)
			if strings.HasPrefix(partMediaType, "image/") && contentID != "" {
				contentID = strings.Trim(contentID, "<>")
				body, err := io.ReadAll(part.Body)
				if err == nil {
					// Extract filename from Content-Disposition or use Content-ID
					filename := contentID
					if contentDisposition != "" {
						// Try to extract filename from Content-Disposition
						filenameRegex := regexp.MustCompile(`filename="?([^";\s]+)"?`)
						if matches := filenameRegex.FindStringSubmatch(contentDisposition); len(matches) > 1 {
							filename = matches[1]
						}
					}

					attachments = append(attachments, EmailAttachment{
						Filename:  filename,
						Content:   base64.StdEncoding.EncodeToString(body),
						MimeType:  partMediaType,
						ContentID: contentID,
					})
					log.Printf("Extracted inline image attachment: %s (Content-ID: %s)", filename, contentID)
				}
			} else if contentDisposition == "" || !strings.HasPrefix(contentDisposition, "attachment") {
				// Recursively extract text/html from nested parts
				partText, partHTML, partAttachments := extractBodyParts(part)
				if partText != "" && text == "" {
					text = partText
				}
				if partHTML != "" {
					html = partHTML
				}
				attachments = append(attachments, partAttachments...)
			}
		}
	} else if mediaType == "text/plain" {
		body, err := io.ReadAll(entity.Body)
		if err == nil {
			text = string(body)
		}
	} else if mediaType == "text/html" {
		body, err := io.ReadAll(entity.Body)
		if err == nil {
			html = string(body)
		}
	} else {
		charset := params["charset"]
		if charset == "" {
			charset = "utf-8"
		}
		body, err := io.ReadAll(entity.Body)
		if err == nil {
			text = string(body)
		}
	}

	return text, html, attachments
}

// StartSMTPServer starts the SMTP server
func StartSMTPServer(port, relayEndpointURL, relayCredential string, stats *Store) error {
	if port == "" {
		return errors.New("SMTP port is required")
	}

	if relayEndpointURL == "" {
		return errors.New("relay endpoint URL is required")
	}

	if relayCredential == "" {
		return errors.New("relay credential is required")
	}

	backend := NewSMTPBackend(relayEndpointURL, relayCredential, stats)

	server := smtp.NewServer(backend)
	server.Addr = ":" + port
	server.Domain = "smtp.local"
	server.AllowInsecureAuth = true // OK within private Docker network
	server.MaxMessageBytes = MaxEmailSize
	server.MaxRecipients = 50
	server.ReadTimeout = 30 * time.Second
	server.WriteTimeout = 30 * time.Second

	// Check if port is already in use
	testLn, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		return fmt.Errorf("port %s is already in use or cannot bind: %w", port, err)
	}
	testLn.Close()

	// Listen on all interfaces (Docker network)
	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen on port %s: %w", port, err)
	}

	log.Printf("✓ SMTP Server started on port %s", port)
	log.Printf("✓ Forwarding emails to relay backend at %s", relayEndpointURL)
	log.Printf("✓ Ready to accept SMTP connections from apps")

	// Start serving in a goroutine
	go func() {
		if err := server.Serve(ln); err != nil {
			log.Printf("SMTP server error: %v", err)
		}
	}()

	// Give it a moment to start
	time.Sleep(100 * time.Millisecond)

	log.Println("✓ SMTP server is running")
	return nil
}
