package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Saying something when a blade goes bad.
//
// The verdict was computed, coloured and logged, and then it sat on a page
// nobody was looking at. A blade that overheats at three in the morning was
// amber until somebody happened to open the browser.
//
// Two rules keep this from becoming noise, which is the only way a
// notification stays worth reading:
//
//   - A verdict has to *stay* bad. A blade that reboots is briefly offline
//     and briefly hot, and neither is news. Nothing is sent until the verdict
//     has held for the hold time.
//   - What went bad is said once, and what recovered is said once. A blade
//     that is warm for two days sends one mail, not two thousand.

type mailConf struct {
	Enabled bool
	Host    string
	Port    int
	User    string
	Pass    string
	TLS     string // starttls | tls | none
	From    string
	To      string
	Min     string // warn | crit
	HoldMin int
}

func (a *App) mailConf() mailConf {
	port, _ := strconv.Atoi(a.setting("notify_port", "587"))
	if port == 0 {
		port = 587
	}
	hold, _ := strconv.Atoi(a.setting("notify_hold_min", "10"))
	if hold <= 0 {
		hold = 10
	}
	return mailConf{
		Enabled: a.setting("notify_enabled", "") == "1",
		Host:    a.setting("notify_host", ""),
		Port:    port,
		User:    a.setting("notify_user", ""),
		Pass:    a.setting("notify_pass", ""),
		TLS:     a.setting("notify_tls", "starttls"),
		From:    a.setting("notify_from", ""),
		To:      a.setting("notify_to", ""),
		Min:     a.setting("notify_min", "warn"),
		HoldMin: hold,
	}
}

func (m mailConf) ready() bool {
	return m.Host != "" && m.From != "" && m.To != ""
}

// ── The watch ────────────────────────────────────────────────────────

// alert is what is remembered between two passes.
type alert struct {
	Serial   string
	Level    string
	Reason   string
	Since    time.Time
	Notified string // the level that was sent, empty while nothing has been
}

func levelName(l healthLevel) string {
	switch l {
	case hWarn:
		return "warn"
	case hCrit:
		return "crit"
	}
	return "ok"
}

// worthSending says whether this level clears the configured floor.
func worthSending(level, min string) bool {
	if min == "crit" {
		return level == "crit"
	}
	return level == "warn" || level == "crit"
}

// watchHealth runs a pass a minute: judge every blade, remember what is bad,
// and say something once it has been bad long enough.
func (a *App) watchHealth() {
	for {
		time.Sleep(60 * time.Second)
		conf := a.mailConf()
		if err := a.healthPass(conf); err != nil {
			log.Printf("health watch: %v", err)
		}
	}
}

func (a *App) healthPass(conf mailConf) error {
	blades, err := a.listBlades()
	if err != nil {
		return err
	}
	open, err := a.openAlerts()
	if err != nil {
		return err
	}
	now := time.Now().UTC()

	for i := range blades {
		b := &blades[i]
		level, reasons := a.evalHealth(b)
		name := levelName(level)
		cur, had := open[b.Serial]
		delete(open, b.Serial)

		if level == hUnknown {
			// A blade that has never reported is not a blade that went bad.
			continue
		}
		if name == "ok" {
			if had && cur.Notified != "" {
				a.sendAlert(conf, b, "ok", "", cur)
			}
			if had {
				_ = a.clearAlert(b.Serial)
			}
			continue
		}

		reason := joinErr(LangEN, reasons)
		switch {
		case !had:
			_ = a.raiseAlert(alert{Serial: b.Serial, Level: name, Reason: reason, Since: now})
		case cur.Level != name:
			// It got worse or better without recovering: the clock starts
			// again, and a level that was already said may be said again.
			cur.Level, cur.Reason, cur.Since = name, reason, now
			if worse(name, cur.Notified) {
				cur.Notified = ""
			}
			_ = a.raiseAlert(cur)
		default:
			cur.Reason = reason
			if cur.Notified != name && now.Sub(cur.Since) >= time.Duration(conf.HoldMin)*time.Minute {
				a.sendAlert(conf, b, name, reason, cur)
			} else {
				_ = a.raiseAlert(cur)
			}
		}
	}

	// Whatever is left in `open` belongs to a blade that no longer exists.
	for serial := range open {
		_ = a.clearAlert(serial)
	}
	return nil
}

func worse(a, b string) bool {
	rank := map[string]int{"": 0, "ok": 0, "warn": 1, "crit": 2}
	return rank[a] > rank[b]
}

// sendAlert writes the event either way, and mails it when mail is
// configured. The log is the record; the mail is the tap on the shoulder.
func (a *App) sendAlert(conf mailConf, b *Blade, level, reason string, cur alert) {
	subject, body := alertText(b, level, reason, cur)

	if level == "ok" {
		a.logEvent(b.Serial, "info", "recovered: "+b.Hostname+" is well again")
	} else {
		a.logEvent(b.Serial, "warn", "alert: "+b.Hostname+" — "+reason)
	}

	if conf.Enabled && conf.ready() && (level == "ok" || worthSending(level, conf.Min)) {
		if err := sendMail(conf, subject, body); err != nil {
			log.Printf("notification not sent: %v", err)
			a.logEvent(b.Serial, "warn", "notification not sent: "+err.Error())
			// Not marked as notified: the next pass tries again.
			return
		}
	}
	if level == "ok" {
		return
	}
	cur.Serial, cur.Level, cur.Reason, cur.Notified = b.Serial, level, reason, level
	if cur.Since.IsZero() {
		cur.Since = time.Now().UTC()
	}
	_ = a.raiseAlert(cur)
}

func alertText(b *Blade, level, reason string, cur alert) (string, string) {
	where := b.Hostname
	if where == "" {
		where = b.Serial
	}
	if level == "ok" {
		since := ""
		if !cur.Since.IsZero() {
			since = fmt.Sprintf("\n\nIt had been %s since %s.",
				cur.Level, cur.Since.Local().Format("2006-01-02 15:04"))
		}
		return "Sheath: " + where + " is well again",
			where + " reports normal values again." + since + "\n"
	}
	head := "Sheath: " + where + " needs attention"
	if level == "crit" {
		head = "Sheath: " + where + " is in trouble"
	}
	body := fmt.Sprintf("%s (%s)\n\n%s\n\nSince %s.\n",
		where, b.IP, reason, cur.Since.Local().Format("2006-01-02 15:04"))
	if b.RackName != "" {
		where := "BladeRunner " + b.RackName
		if b.Slot != nil {
			where += fmt.Sprintf(", slot %d", *b.Slot)
		}
		if b.SiteName != "" {
			where += ", site " + b.SiteName
		}
		body += "\n" + where + ".\n"
	}
	return head, body
}

// ── Sending ──────────────────────────────────────────────────────────

func sendMail(c mailConf, subject, body string) error {
	if !c.ready() {
		return fmt.Errorf("mail is not configured")
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	msg := buildMessage(c, subject, body)

	var cl *smtp.Client
	var err error
	switch c.TLS {
	case "tls":
		// Implicit TLS, port 465: the connection is encrypted before a byte
		// of SMTP is spoken.
		conn, derr := tls.Dial("tcp", addr, &tls.Config{ServerName: c.Host})
		if derr != nil {
			return derr
		}
		cl, err = smtp.NewClient(conn, c.Host)
	default:
		cl, err = smtp.Dial(addr)
	}
	if err != nil {
		return err
	}
	defer cl.Close()

	if c.TLS == "starttls" {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%s does not offer STARTTLS", c.Host)
		}
		if err := cl.StartTLS(&tls.Config{ServerName: c.Host}); err != nil {
			return err
		}
	}
	if c.User != "" {
		// PlainAuth refuses to speak over an unencrypted link, which is the
		// right refusal — it would otherwise put the password on the wire.
		if err := cl.Auth(smtp.PlainAuth("", c.User, c.Pass, c.Host)); err != nil {
			return err
		}
	}
	if err := cl.Mail(c.From); err != nil {
		return err
	}
	for _, to := range recipients(c.To) {
		if err := cl.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}

func recipients(to string) []string {
	parts := strings.FieldsFunc(to, func(r rune) bool { return r == ',' || r == ';' || r == ' ' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func buildMessage(c mailConf, subject, body string) []byte {
	var b strings.Builder
	b.WriteString("From: Sheath <" + c.From + ">\r\n")
	b.WriteString("To: " + strings.Join(recipients(c.To), ", ") + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	// A line that begins with a dot ends the message early if it goes out
	// unescaped. net/smtp's writer handles that, but the body is ours.
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return []byte(b.String())
}
