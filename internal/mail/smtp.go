package mail

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/orchard-suite/dispatch/internal/entra"
	"github.com/orchard-suite/dispatch/internal/notice"
)

// SMTP sends through one SMTP server, signed in with the URL's user and
// password. smtp:// upgrades with STARTTLS, which it requires before it
// signs in; smtps:// is TLS from the first byte.
type SMTP struct {
	host     string
	addr     string
	implicit bool
	user     string
	pass     string
	from     string
	// TLS is the client's TLS settings; tests trust their own server.
	TLS *tls.Config
}

func NewSMTP(raw, from string) (*SMTP, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// The URL holds the password; the parse error would repeat it.
		return nil, errors.New("mail: MAIL_SMTP_URL is not a URL")
	}
	if u.Scheme != "smtp" && u.Scheme != "smtps" || u.Hostname() == "" {
		return nil, errors.New("mail: MAIL_SMTP_URL is smtp://user:pass@host:587 or smtps://user:pass@host:465")
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"smtp": "587", "smtps": "465"}[u.Scheme]
	}
	pass, _ := u.User.Password()
	return &SMTP{host: u.Hostname(), addr: net.JoinHostPort(u.Hostname(), port), implicit: u.Scheme == "smtps",
		user: u.User.Username(), pass: pass, from: from, TLS: &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}}, nil
}

func (s *SMTP) Kind() string { return "smtp" }

// Send speaks one SMTP conversation. The status is HTTP-like: 202 taken,
// 400 for a permanent refusal (a 5xx reply), 0 for anything worth trying
// again.
func (s *SMTP) Send(ctx context.Context, to string, l notice.Letter) (int, error) {
	err := s.send(ctx, to, l)
	if err == nil {
		return 202, nil
	}
	var reply *textproto.Error
	if errors.As(err, &reply) && reply.Code >= 500 {
		return 400, fmt.Errorf("the mail server refused it: %d %s", reply.Code, entra.FirstLine(reply.Msg))
	}
	return 0, err
}

func (s *SMTP) send(ctx context.Context, to string, l notice.Letter) error {
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialer := &net.Dialer{Deadline: deadline}
	var conn net.Conn
	var err error
	if s.implicit {
		conn, err = tls.DialWithDialer(dialer, "tcp", s.addr, s.TLS)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", s.addr)
	}
	if err != nil {
		return fmt.Errorf("the mail server: %w", err)
	}
	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if err := c.Hello("dispatch"); err != nil {
		return err
	}
	if !s.implicit {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(s.TLS); err != nil {
				return fmt.Errorf("STARTTLS: %w", err)
			}
		} else if s.user != "" {
			return errors.New("the mail server offers no STARTTLS, and the password is not sent in the clear")
		}
	}
	if s.user != "" {
		if err := c.Auth(smtp.PlainAuth("", s.user, s.pass, s.host)); err != nil {
			return err
		}
	}
	if err := c.Mail(s.from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(s.message(to, l)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// message is the letter as MIME: a plain text part and an HTML part.
func (s *SMTP) message(to string, l notice.Letter) []byte {
	var raw [12]byte
	_, _ = rand.Read(raw[:])
	boundary := "dispatch-" + hex.EncodeToString(raw[:])
	var b strings.Builder
	head := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	head("From", s.from)
	head("To", to)
	head("Subject", mime.QEncoding.Encode("utf-8", l.Subject))
	head("Date", time.Now().UTC().Format(time.RFC1123Z))
	head("Message-ID", "<"+hex.EncodeToString(raw[:])+"@dispatch>")
	head("MIME-Version", "1.0")
	head("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	part := func(kind, body string) {
		b.WriteString("--" + boundary + "\r\n")
		head("Content-Type", kind+"; charset=utf-8")
		head("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		qp := quotedprintable.NewWriter(&b)
		_, _ = qp.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n")))
		_ = qp.Close()
		b.WriteString("\r\n")
	}
	part("text/plain", l.Text)
	part("text/html", l.HTML)
	b.WriteString("--" + boundary + "--\r\n")
	return []byte(b.String())
}
