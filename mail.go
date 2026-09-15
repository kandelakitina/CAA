package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type mailConfig struct{ Host, Port, Mode, Username, Password, From, BaseURL string }

func loadMailConfig() (*mailConfig, error) {
	if os.Getenv("SMTP_HOST") == "" {
		return nil, nil
	}
	c := &mailConfig{Host: os.Getenv("SMTP_HOST"), Port: os.Getenv("SMTP_PORT"), Mode: os.Getenv("SMTP_SECURITY"), Username: os.Getenv("SMTP_USERNAME"), Password: os.Getenv("SMTP_PASSWORD"), From: os.Getenv("SMTP_FROM"), BaseURL: strings.TrimRight(os.Getenv("APP_BASE_URL"), "/")}
	if c.Port == "" {
		c.Port = "465"
	}
	if c.Mode == "" {
		c.Mode = "tls"
	}
	port, err := strconv.Atoi(c.Port)
	if err != nil || port < 1 || port > 65535 || strings.ContainsAny(c.Host, "\r\n /:") {
		return nil, errors.New("Некорректные SMTP_HOST или SMTP_PORT")
	}
	if c.Mode != "tls" && c.Mode != "starttls" {
		return nil, errors.New("SMTP_SECURITY: допустимы tls или starttls")
	}
	if !validMailAddress(c.From) || !validMailAddress(c.Username) || c.Password == "" {
		return nil, errors.New("Укажите SMTP_FROM, SMTP_USERNAME (адреса ящиков) и SMTP_PASSWORD")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, errors.New("APP_BASE_URL должен содержать публичный HTTPS-адрес приложения без пути")
	}
	return c, nil
}
func validMailAddress(s string) bool {
	a, e := mail.ParseAddress(s)
	return e == nil && a.Address == s && !strings.ContainsAny(s, "\r\n")
}

func (app *application) migrateMail(ctx context.Context) error {
	_, err := app.db.Exec(ctx, `
 CREATE TABLE IF NOT EXISTS email_password_tokens (
 id BIGSERIAL PRIMARY KEY, user_id BIGINT NOT NULL REFERENCES users(id),
 token_hash TEXT NOT NULL UNIQUE, password_snapshot TEXT NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL, used_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
 );
 CREATE INDEX IF NOT EXISTS email_password_tokens_user_idx ON email_password_tokens(user_id);
 CREATE TABLE IF NOT EXISTS email_outbox (
 id BIGSERIAL PRIMARY KEY, user_id BIGINT NOT NULL REFERENCES users(id),
 subject TEXT NOT NULL, encrypted_body BYTEA, token_id BIGINT REFERENCES email_password_tokens(id),
 attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), completed_at TIMESTAMPTZ, outcome TEXT NOT NULL DEFAULT 'pending'
 );
 CREATE INDEX IF NOT EXISTS email_outbox_pending_idx ON email_outbox(next_attempt_at,id) WHERE completed_at IS NULL;
 `)
	return err
}

// Encrypt pending links so they cannot be used from a database dump alone.
func (app *application) mailCipher() (cipher.AEAD, error) {
	key := sha256.Sum256(append([]byte("neva-email-v1:"), app.sessionSecret...))
	b, e := aes.NewCipher(key[:])
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(b)
}
func (app *application) encryptMail(body string) ([]byte, error) {
	a, e := app.mailCipher()
	if e != nil {
		return nil, e
	}
	n := make([]byte, a.NonceSize())
	if _, e = rand.Read(n); e != nil {
		return nil, e
	}
	return a.Seal(n, n, []byte(body), nil), nil
}
func (app *application) decryptMail(body []byte) (string, error) {
	a, e := app.mailCipher()
	if e != nil {
		return "", e
	}
	if len(body) < a.NonceSize() {
		return "", errors.New("invalid encrypted email")
	}
	b, e := a.Open(nil, body[:a.NonceSize()], body[a.NonceSize():], nil)
	return string(b), e
}
func (app *application) queueMail(ctx context.Context, tx auditExecutor, userID int64, subject, body string, tokenID *int64) error {
	encrypted, err := app.encryptMail(body)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO email_outbox(user_id,subject,encrypted_body,token_id) VALUES($1,$2,$3,$4)`, userID, subject, encrypted, tokenID)
	return err
}

func smtpMessage(from, to, subject, body, messageID string) ([]byte, error) {
	if !validMailAddress(from) || !validMailAddress(to) || strings.ContainsAny(subject+messageID, "\r\n") {
		return nil, errors.New("invalid email headers")
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n", from, to, mime.QEncoding.Encode("UTF-8", subject), time.Now().Format(time.RFC1123Z), messageID)
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.Bytes(), nil
}
func (cfg *mailConfig) send(ctx context.Context, to, subject, body, id string) error {
	return cfg.sendTLS(ctx, to, subject, body, id, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
}

func (cfg *mailConfig) sendTLS(ctx context.Context, to, subject, body, id string, tc *tls.Config) error {
	message, err := smtpMessage(cfg.From, to, subject, body, id)
	if err != nil {
		return err
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, cfg.Port))
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(20 * time.Second)
	if t, ok := ctx.Deadline(); ok && t.Before(deadline) {
		deadline = t
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return err
	}
	if cfg.Mode == "tls" {
		secured := tls.Client(conn, tc)
		if err = secured.HandshakeContext(ctx); err != nil {
			return err
		}
		conn = secured
	}
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	if cfg.Mode == "starttls" {
		if supported, _ := client.Extension("STARTTLS"); !supported {
			return errors.New("SMTP server does not support STARTTLS")
		}
		if err = client.StartTLS(tc); err != nil {
			return err
		}
	}
	if err = client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
		return err
	}
	if err = client.Mail(cfg.From); err != nil {
		return err
	}
	if err = client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = w.Write(message); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	// DATA acceptance is success even if the connection closes before QUIT.
	_ = client.Quit()
	return nil
}
func (app *application) runMailQueue() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		for i := 0; i < 20; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			worked, err := app.deliverNextMail(ctx)
			cancel()
			if err != nil {
				log.Print("email queue: processing failed; will retry")
				break
			}
			if !worked {
				break
			}
		}
		<-ticker.C
	}
}
func (app *application) deliverNextMail(ctx context.Context) (bool, error) {
	tx, err := app.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id, userID int64
	var subject string
	var encrypted []byte
	var tokenID *int64
	err = tx.QueryRow(ctx, `SELECT id,user_id,subject,encrypted_body,token_id FROM email_outbox WHERE completed_at IS NULL AND next_attempt_at<=NOW() ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id, &userID, &subject, &encrypted, &tokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var to string
	var active bool
	err = tx.QueryRow(ctx, `SELECT email,active FROM users WHERE id=$1`, userID).Scan(&to, &active)
	if err != nil {
		return false, err
	}
	valid := active
	if valid && tokenID != nil {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM email_password_tokens t JOIN users u ON u.id=t.user_id WHERE t.id=$1 AND t.used_at IS NULL AND t.expires_at>NOW() AND t.password_snapshot=u.password_hash)`, *tokenID).Scan(&valid)
		if err != nil {
			return false, err
		}
	}
	outcome := "cancelled"
	if valid {
		body, e := app.decryptMail(encrypted)
		if e == nil {
			u, _ := url.Parse(app.mail.BaseURL)
			e = app.mail.send(ctx, to, subject, body, fmt.Sprintf("neva-%d@%s", id, u.Hostname()))
		}
		if e != nil {
			_, err = tx.Exec(ctx, `UPDATE email_outbox SET attempts=attempts+1,next_attempt_at=NOW()+make_interval(secs=>LEAST(3600,30*power(2,LEAST(attempts,7)))::double precision),outcome='retry' WHERE id=$1`, id)
			if err != nil {
				return false, err
			}
			log.Printf("email queue: message %d failed; scheduled retry", id)
			return true, tx.Commit(ctx)
		}
		outcome = "sent"
	}
	_, err = tx.Exec(ctx, `UPDATE email_outbox SET completed_at=NOW(),outcome=$2,encrypted_body=NULL,attempts=attempts+1 WHERE id=$1`, id, outcome)
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
