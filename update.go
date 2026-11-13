package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Check for updates: asks GitHub for the latest published release (not a
// pre-release) and compares it with this build. It never downloads or
// installs anything; the About tab offers a link to the release page.

const releasesAPI = "https://api.github.com/repos/va3mw/FMTAssistant/releases/latest"

// UpdateInfo is the result of a check for updates.
type UpdateInfo struct {
	Current     string `json:"current"`
	Latest      string `json:"latest"`
	Newer       bool   `json:"newer"`        // Latest is newer than this build
	URL         string `json:"url"`          // release page
	DownloadURL string `json:"download_url"` // FMTAssistant.exe, if attached
	Published   string `json:"published"`    // YYYY-MM-DD
	Notes       string `json:"notes"`
}

// CheckForUpdate asks GitHub whether a newer release is out.
func (a *App) CheckForUpdate() (UpdateInfo, error) {
	info := UpdateInfo{Current: version}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesAPI, nil)
	if err != nil {
		return info, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "FMTAssistant/"+version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return info, fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var rel struct {
		TagName     string    `json:"tag_name"`
		HTMLURL     string    `json:"html_url"`
		Body        string    `json:"body"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return info, fmt.Errorf("reading GitHub's answer: %w", err)
	}
	info.Latest = strings.TrimPrefix(rel.TagName, "v")
	info.URL = rel.HTMLURL
	info.Notes = strings.TrimSpace(rel.Body)
	if !rel.PublishedAt.IsZero() {
		info.Published = rel.PublishedAt.UTC().Format("2006-01-02")
	}
	for _, as := range rel.Assets {
		if strings.EqualFold(as.Name, "FMTAssistant.exe") {
			info.DownloadURL = as.URL
		}
	}
	info.Newer = versionNewer(info.Latest, version)
	a.src.Info("update check: running %s, latest release %s", version, info.Latest)
	return info, nil
}

// versionNewer reports whether version a (e.g. "0.3.10") is newer than b.
// Anything after a hyphen (a test suffix) is ignored.
func versionNewer(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func versionParts(v string) []int {
	v, _, _ = strings.Cut(strings.TrimPrefix(strings.TrimSpace(v), "v"), "-")
	var out []int
	for _, f := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(f)
		out = append(out, n)
	}
	return out
}
