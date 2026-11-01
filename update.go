package main

// Check for updates.
//
// This is the one place DiskScout talks to the internet, and it happens only when the user presses "Check for updates" in
// the window's menu: never at start-up, never in the background, never on a timer, and Claude has no tool for it. It asks
// GitHub for the newest published release and compares the version number. It downloads and installs nothing.
//
// What GitHub can see is what any request shows: the IP address, and a User-Agent that says "DiskScout/<this version>".
// Nothing about the machine, the disk or the files is sent. The page it offers to open is a fixed address, never one taken
// from GitHub's reply, and the reply is read only for its version number.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

const (
	releasesAPI  = "https://api.github.com/repos/agentruntime-io/diskscout/releases/latest"
	releasesPage = "https://github.com/agentruntime-io/diskscout/releases/latest"
)

// updateAPI, updateTimeout and openURL are variables only so that tests can stand in for GitHub and for the browser. The
// limit is generous on purpose: the first request on a slow network (a cold DNS lookup, say) can take ten seconds or more.
var (
	updateAPI     = releasesAPI
	updateTimeout = 20 * time.Second
)

var openURL = func(u string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
}

type UpdateInfo struct {
	Current string `json:"current"`
	Latest  string `json:"latest"`
	Newer   bool   `json:"newer"` // a release newer than this one exists
}

// A release is named like v1.8.0. Anything else is refused rather than guessed at.
var versionRe = regexp.MustCompile(`^v?(\d{1,4})\.(\d{1,4})\.(\d{1,4})$`)

func parseVersion(s string) ([3]int, bool) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

func versionNewer(latest, current [3]int) bool {
	for i := range latest {
		if latest[i] != current[i] {
			return latest[i] > current[i]
		}
	}
	return false
}

func versionString(v [3]int) string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// checkForUpdate makes one request to GitHub. Every failure comes back as a sentence the window can show as it is.
func checkForUpdate(ctx context.Context, current string) (UpdateInfo, error) {
	cur, ok := parseVersion(current)
	if !ok {
		return UpdateInfo{}, fmt.Errorf("this build is called %q, which cannot be compared with a release number", current)
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, updateAPI, nil)
	if err != nil {
		return UpdateInfo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "DiskScout/"+versionString(cur))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("update check: %v", err)
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return UpdateInfo{}, errors.New("GitHub did not answer in time. The network may be slow: try again")
		}
		return UpdateInfo{}, errors.New("could not reach GitHub. Are you online?")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return UpdateInfo{}, errors.New("no release has been published yet")
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return UpdateInfo{}, errors.New("GitHub is limiting requests from this network for now. Try again later")
	case resp.StatusCode != http.StatusOK:
		return UpdateInfo{}, fmt.Errorf("GitHub answered with an error (%d)", resp.StatusCode)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return UpdateInfo{}, errors.New("GitHub's answer was not understood")
	}
	latest, ok := parseVersion(rel.TagName)
	if !ok {
		return UpdateInfo{}, errors.New("the newest release has a name this version cannot read as a version number")
	}
	return UpdateInfo{Current: versionString(cur), Latest: versionString(latest), Newer: versionNewer(latest, cur)}, nil
}
