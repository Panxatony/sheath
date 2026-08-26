package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Signing in with a code instead of being handed a token.
//
// The permanent token used to be generated at the centre and carried here by
// hand — through a clipboard, a shell history and whatever else was in the
// way. This asks for it instead: a code that is good once and for an hour,
// exchanged for the token, which is written straight into a file this machine
// owns and never appears on a command line.
//
// It runs once. After that the token is on disk and the code is spent, so the
// flag can stay in the unit file without doing anything on every restart.

type enrolment struct {
	SiteID  int64  `json:"site_id"`
	Name    string `json:"name"`
	Token   string `json:"token"`
	NetBase string `json:"net_base"`
	Server  string `json:"server"`
}

// enroll exchanges a code for a token and writes both it and the site id
// beside each other, so the next start needs neither flag.
func enroll(server, code, tokenFile string) (*enrolment, error) {
	host, _ := os.Hostname()
	body, _ := json.Marshal(map[string]string{"code": code, "hostname": host})
	req, err := http.NewRequest("POST", strings.TrimRight(server, "/")+"/api/v1/site/enroll",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return nil, fmt.Errorf("enrollment refused: %s", e.Error)
	}
	var out enrolment
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Token == "" || out.SiteID == 0 {
		return nil, fmt.Errorf("the centre answered without a token")
	}

	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		return nil, err
	}
	// 0600 from the first moment: a token that is briefly world-readable was
	// briefly readable by the world.
	if err := os.WriteFile(tokenFile, []byte(out.Token+"\n"), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(siteIDPath(tokenFile),
		[]byte(strconv.FormatInt(out.SiteID, 10)+"\n"), 0o644); err != nil {
		log.Printf("site id not written: %v", err)
	}
	return &out, nil
}

// siteIDPath is the file beside the token that says which site this is. Kept
// separate from the token so it can be read by anyone debugging without
// handing them the credential.
func siteIDPath(tokenFile string) string {
	return filepath.Join(filepath.Dir(tokenFile), "site-id")
}

func readSiteID(tokenFile string) int64 {
	b, err := os.ReadFile(siteIDPath(tokenFile))
	if err != nil {
		return 0
	}
	id, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return id
}
