package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
)

// lucidaSessionCache holds the cf_clearance cookie and matching User-Agent
// obtained from FlareSolverr. Both fields must be used together — Cloudflare
// ties the clearance token to the exact UA that solved the challenge.
type lucidaSessionCache struct {
	mu        sync.Mutex
	cookie    string // cf_clearance value
	userAgent string
}

var lucidaSession = &lucidaSessionCache{}

func flareSolverrURL() string {
	if u := os.Getenv("FLARESOLVERR_URL"); u != "" {
		return u
	}
	return "http://100.80.119.94:8191"
}

type flareSolverrRequest struct {
	Cmd        string `json:"cmd"`
	URL        string `json:"url"`
	MaxTimeout int    `json:"maxTimeout"`
}

type flareSolverrResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Solution struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
		UserAgent string `json:"userAgent"`
	} `json:"solution"`
}

// solveLucida calls FlareSolverr and stores fresh credentials in the cache.
// Caller must hold lucidaSession.mu.
func solveLucida() (cookie, userAgent string, err error) {
	endpoint := flareSolverrURL() + "/v1"
	payload := flareSolverrRequest{
		Cmd:        "request.get",
		URL:        "https://lucida.to/",
		MaxTimeout: 60000,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("FlareSolverr unreachable: %w", err)
	}
	defer resp.Body.Close()

	var fsResp flareSolverrResponse
	if err := json.NewDecoder(resp.Body).Decode(&fsResp); err != nil {
		return "", "", fmt.Errorf("failed to decode FlareSolverr response: %w", err)
	}
	if fsResp.Status != "ok" {
		return "", "", fmt.Errorf("FlareSolverr returned status %q: %s", fsResp.Status, fsResp.Message)
	}

	for _, c := range fsResp.Solution.Cookies {
		if c.Name == "cf_clearance" {
			cookie = c.Value
			break
		}
	}
	if cookie == "" {
		return "", "", fmt.Errorf("FlareSolverr did not return a cf_clearance cookie")
	}

	userAgent = fsResp.Solution.UserAgent
	lucidaSession.cookie = cookie
	lucidaSession.userAgent = userAgent

	fmt.Printf("[FlareSolverr] Got cf_clearance (UA: %.80s...)\n", userAgent)
	return cookie, userAgent, nil
}

// getLucidaSession returns a valid cf_clearance and matching User-Agent,
// solving via FlareSolverr on first call (or after invalidation).
func getLucidaSession() (cookie, userAgent string, err error) {
	lucidaSession.mu.Lock()
	defer lucidaSession.mu.Unlock()

	if lucidaSession.cookie != "" {
		return lucidaSession.cookie, lucidaSession.userAgent, nil
	}

	fmt.Println("[FlareSolverr] Solving Cloudflare challenge for lucida.to...")
	return solveLucida()
}

// refreshLucidaSession invalidates the current session and immediately
// re-solves, returning fresh credentials. Call when a 403 CF challenge
// is detected on a previously-cleared session.
func refreshLucidaSession() (cookie, userAgent string, err error) {
	lucidaSession.mu.Lock()
	defer lucidaSession.mu.Unlock()

	lucidaSession.cookie = ""
	lucidaSession.userAgent = ""
	fmt.Println("[FlareSolverr] CF clearance expired — re-solving...")
	return solveLucida()
}
