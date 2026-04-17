package backend

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type DeezerDownloader struct {
	client *http.Client
}

func NewDeezerDownloader() *DeezerDownloader {
	return &DeezerDownloader{
		client: &http.Client{
			Timeout: 0, // No overall timeout for large file downloads
		},
	}
}

func (d *DeezerDownloader) getRandomUserAgent() string {
	return fmt.Sprintf("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_%d_%d) AppleWebKit/%d.%d (KHTML, like Gecko) Chrome/%d.0.%d.%d Safari/%d.%d",
		rand.Intn(4)+11, rand.Intn(5)+4,
		rand.Intn(7)+530, rand.Intn(7)+30,
		rand.Intn(25)+80, rand.Intn(1500)+3000, rand.Intn(65)+60,
		rand.Intn(7)+530, rand.Intn(6)+30)
}

func (d *DeezerDownloader) extractData(html string, patterns []string) string {
	for _, p := range patterns {
		re := regexp.MustCompile(p)
		matches := re.FindStringSubmatch(html)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

// SearchTrackForISRC finds the Deezer URL and ISRC for a track via the public Deezer
// search API (no auth, no Odesli). The search uses artist + title; the first result's
// track detail is fetched to obtain the canonical ISRC.
func (d *DeezerDownloader) SearchTrackForISRC(artist, title string) (deezerURL, isrc string, err error) {
	client := &http.Client{Timeout: 10 * time.Second}

	searchURL := fmt.Sprintf("https://api.deezer.com/search?q=%s&limit=1",
		url.QueryEscape(artist+" "+title))

	resp, err := client.Get(searchURL)
	if err != nil {
		return "", "", fmt.Errorf("deezer search failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("deezer search returned status %d", resp.StatusCode)
	}

	var searchResp struct {
		Data []struct {
			ID   int64  `json:"id"`
			Link string `json:"link"`
		} `json:"data"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return "", "", fmt.Errorf("failed to decode deezer search response: %w", err)
	}
	if len(searchResp.Data) == 0 {
		return "", "", fmt.Errorf("no deezer results for %q by %q", title, artist)
	}

	trackID := searchResp.Data[0].ID
	deezerURL = searchResp.Data[0].Link

	// Fetch full track detail to get the ISRC (not included in search results).
	detailResp, err := client.Get(fmt.Sprintf("https://api.deezer.com/track/%d", trackID))
	if err != nil {
		// Return the URL even without ISRC.
		return deezerURL, "", nil
	}
	defer detailResp.Body.Close()

	var detail struct {
		ID   int64  `json:"id"`
		ISRC string `json:"isrc"`
		Link string `json:"link"`
	}
	if err := json.NewDecoder(detailResp.Body).Decode(&detail); err != nil {
		return deezerURL, "", nil
	}
	if detail.Link != "" {
		deezerURL = detail.Link
	}
	return deezerURL, detail.ISRC, nil
}

// GetDeezerURLFromISRC resolves an ISRC directly to a Deezer URL using the public
// Deezer API, bypassing Odesli entirely.
func (d *DeezerDownloader) GetDeezerURLFromISRC(isrc string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	apiURL := fmt.Sprintf("https://api.deezer.com/2.0/track/isrc:%s", isrc)

	resp, err := client.Get(apiURL)
	if err != nil {
		return "", fmt.Errorf("failed to call Deezer ISRC API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Deezer ISRC API returned status %d", resp.StatusCode)
	}

	var track struct {
		ID    int64  `json:"id"`
		Link  string `json:"link"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&track); err != nil {
		return "", fmt.Errorf("failed to decode Deezer ISRC response: %w", err)
	}
	if track.Error != nil {
		return "", fmt.Errorf("Deezer ISRC API error: %s", track.Error.Message)
	}
	if track.Link == "" && track.ID == 0 {
		return "", fmt.Errorf("no Deezer track found for ISRC %s", isrc)
	}
	if track.Link != "" {
		fmt.Printf("Found Deezer URL via ISRC: %s\n", track.Link)
		return track.Link, nil
	}
	url := fmt.Sprintf("https://www.deezer.com/track/%d", track.ID)
	fmt.Printf("Found Deezer URL via ISRC: %s\n", url)
	return url, nil
}

// GetDeezerURLFromSpotify resolves a Spotify track ID to a Deezer URL via song.link.
func (d *DeezerDownloader) GetDeezerURLFromSpotify(spotifyTrackID string) (string, error) {
	return NewSongLinkClient().GetDeezerURLFromSpotify(spotifyTrackID)
}

// DownloadFromLucida drives the lucida.to handshake and streams the file to disk.
// The flow mirrors the Amazon backend: scrape token/streamURL from the landing page,
// POST to the load endpoint, poll for completion, then download.
func (d *DeezerDownloader) DownloadFromLucida(deezerURL, outputDir, quality string) (string, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Transport: tr,
		Jar:       jar,
		Timeout:   0,
	}

	userAgent := d.getRandomUserAgent()

	// Step 1: Load the lucida landing page to extract token, stream URL, and token expiry.
	fmt.Printf("Initializing lucida for Deezer... (Target: %s)\n", deezerURL)
	lucidaBase, _ := base64.StdEncoding.DecodeString("aHR0cHM6Ly9sdWNpZGEudG8vP3VybD0lcyZjb3VudHJ5PWF1dG8=")
	lucidaURL := fmt.Sprintf(string(lucidaBase), url.QueryEscape(deezerURL))
	req, _ := http.NewRequest("GET", lucidaURL, nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	html := string(bodyBytes)

	token := d.extractData(html, []string{`token:"([^"]+)"`, `"token"\s*:\s*"([^"]+)"`})
	streamURL := d.extractData(html, []string{`"url":"([^"]+)"`, `url:"([^"]+)"`})
	expiry := d.extractData(html, []string{`tokenExpiry:(\d+)`, `"tokenExpiry"\s*:\s*(\d+)`})

	if token == "" || streamURL == "" {
		errorMsg := d.extractData(html, []string{`error:"([^"]+)"`, `"error"\s*:\s*"([^"]+)"`})
		if errorMsg != "" {
			return "", fmt.Errorf("lucida error: %s", errorMsg)
		}
		return "", fmt.Errorf("could not extract required data from lucida")
	}

	// The token is double-base64 encoded; decode it twice to get the raw value.
	decodedToken := token
	if secondBase64, err := base64.StdEncoding.DecodeString(token); err == nil {
		if firstBase64, err := base64.StdEncoding.DecodeString(string(secondBase64)); err == nil {
			decodedToken = string(firstBase64)
		}
	}

	streamURL = strings.ReplaceAll(streamURL, `\/`, `/`)
	fmt.Println("Fetching Deezer stream via Lucida...")

	// Step 2: POST the load request to initiate server-side processing.
	loadPayload := map[string]interface{}{
		"account":   map[string]string{"id": "auto", "type": "country"},
		"compat":    "false",
		"downscale": "original",
		"handoff":   true,
		"metadata":  true,
		"private":   true,
		"token":     map[string]interface{}{"primary": decodedToken, "expiry": expiry},
		"upload":    map[string]bool{"enabled": false},
		"url":       streamURL,
	}

	payloadBytes, _ := json.Marshal(loadPayload)
	loadAPI, _ := base64.StdEncoding.DecodeString("aHR0cHM6Ly9sdWNpZGEudG8vYXBpL2xvYWQ/dXJsPS9hcGkvZmV0Y2gvc3RyZWFtL3Yy")
	req, _ = http.NewRequest("POST", string(loadAPI), bytes.NewBuffer(payloadBytes))
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, */*;q=0.5")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	req.Header.Set("Origin", "https://lucida.to")
	req.Header.Set("Referer", lucidaURL)

	for _, cookie := range client.Jar.Cookies(req.URL) {
		if cookie.Name == "csrf_token" {
			req.Header.Set("X-CSRF-Token", cookie.Value)
		}
	}

	resp, err = client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var loadData LucidaLoadResponse
	json.NewDecoder(resp.Body).Decode(&loadData)

	if !loadData.Success {
		return "", fmt.Errorf("lucida load request failed: %s", loadData.Error)
	}

	// Step 3: Poll the per-server completion endpoint until the track is ready.
	serviceBase, _ := base64.StdEncoding.DecodeString("aHR0cHM6Ly8=")
	completionBase, _ := base64.StdEncoding.DecodeString("Lmx1Y2lkYS50by9hcGkvZmV0Y2gvcmVxdWVzdC8=")
	completionURL := fmt.Sprintf("%s%s%s%s", string(serviceBase), loadData.Server, string(completionBase), loadData.Handoff)
	fmt.Println("Processing on Lucida server...")

	var finalStatus LucidaStatusResponse
	for {
		req, _ = http.NewRequest("GET", completionURL, nil)
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json, */*;q=0.5")
		req.Header.Set("Accept-Language", "en-US,en;q=0.5")
		req.Header.Set("Referer", "https://lucida.to/")
		resp, err = client.Do(req)
		if err != nil {
			return "", err
		}

		json.NewDecoder(resp.Body).Decode(&finalStatus)
		resp.Body.Close()

		if finalStatus.Status == "completed" {
			fmt.Println("\nTrack processing completed!")
			break
		} else if finalStatus.Status == "error" {
			return "", fmt.Errorf("lucida processing failed: %s", finalStatus.Message)
		} else if finalStatus.Progress.Total > 0 {
			percent := (finalStatus.Progress.Current * 100) / finalStatus.Progress.Total
			fmt.Printf("\rLucida Progress: %d%%", percent)
		}
		time.Sleep(2 * time.Second)
	}

	// Step 4: Download the processed file.
	downloadSuffix, _ := base64.StdEncoding.DecodeString("L2Rvd25sb2Fk")
	downloadURL := fmt.Sprintf("%s%s%s%s%s", string(serviceBase), loadData.Server, string(completionBase), loadData.Handoff, string(downloadSuffix))
	req, _ = http.NewRequest("GET", downloadURL, nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Referer", "https://lucida.to/")
	resp, err = client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("lucida download failed with status %d", resp.StatusCode)
	}

	// Derive filename from Content-Disposition, falling back to a safe default.
	fileName := "track.flac"
	if contentDisp := resp.Header.Get("Content-Disposition"); contentDisp != "" {
		re := regexp.MustCompile(`filename[*]?=([^;]+)`)
		if matches := re.FindStringSubmatch(contentDisp); len(matches) > 1 {
			rawName := strings.Trim(matches[1], `"'`)
			if strings.HasPrefix(rawName, "UTF-8''") {
				if decoded, err := url.PathUnescape(rawName[7:]); err == nil {
					fileName = decoded
				}
			} else {
				fileName = rawName
			}
			reg := regexp.MustCompile(`[<>:"/\\|?*]`)
			fileName = reg.ReplaceAllString(fileName, "")
		}
	}

	filePath := filepath.Join(outputDir, fileName)
	out, err := os.Create(filePath)
	if err != nil {
		return "", err
	}
	defer out.Close()

	fmt.Printf("Downloading from Lucida: %s\n", fileName)
	pw := NewProgressWriter(out)
	if _, err = io.Copy(pw, resp.Body); err != nil {
		out.Close()
		os.Remove(filePath)
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	fmt.Printf("\rDownloaded: %.2f MB (Complete)\n", float64(pw.GetTotal())/(1024*1024))
	return filePath, nil
}

// DownloadBySpotifyID is the main entry point: resolves the Spotify ID to a Deezer URL,
// downloads via lucida.to, renames the file, and embeds Spotify metadata.
// If isrc is non-empty the Deezer URL is resolved directly via the Deezer public API,
// bypassing Odesli; Odesli is used as a fallback when the ISRC lookup fails.
func (d *DeezerDownloader) DownloadBySpotifyID(spotifyTrackID, isrc, outputDir, quality, filenameFormat string, includeTrackNumber bool, position int, spotifyTrackName, spotifyArtistName, spotifyAlbumName, spotifyAlbumArtist, spotifyReleaseDate string, useAlbumTrackNumber bool, spotifyCoverURL string, embedMaxQualityCover bool, spotifyTrackNumber, spotifyDiscNumber, spotifyTotalTracks int, spotifyTotalDiscs int, spotifyCopyright, spotifyPublisher, spotifyURL string) (string, error) {
	if outputDir != "." {
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	// Short-circuit if the file already exists under the expected name.
	if spotifyTrackName != "" && spotifyArtistName != "" {
		expectedFilename := BuildExpectedFilename(spotifyTrackName, spotifyArtistName, spotifyAlbumName, spotifyAlbumArtist, spotifyReleaseDate, filenameFormat, includeTrackNumber, position, spotifyDiscNumber, useAlbumTrackNumber)
		expectedPath := filepath.Join(outputDir, expectedFilename)
		if fileInfo, err := os.Stat(expectedPath); err == nil && fileInfo.Size() > 0 {
			fmt.Printf("File already exists: %s (%.2f MB)\n", expectedPath, float64(fileInfo.Size())/(1024*1024))
			return "EXISTS:" + expectedPath, nil
		}
	}

	var deezerURL string
	var err error
	if isrc != "" {
		deezerURL, err = d.GetDeezerURLFromISRC(isrc)
		if err != nil {
			fmt.Printf("ISRC lookup failed (%v), falling back to Odesli...\n", err)
			deezerURL, err = d.GetDeezerURLFromSpotify(spotifyTrackID)
			if err != nil {
				return "", fmt.Errorf("failed to get Deezer URL: %w", err)
			}
		}
	} else {
		deezerURL, err = d.GetDeezerURLFromSpotify(spotifyTrackID)
		if err != nil {
			return "", fmt.Errorf("failed to get Deezer URL: %w", err)
		}
	}

	fmt.Printf("Using Deezer URL: %s\n", deezerURL)

	filePath, err := d.DownloadFromLucida(deezerURL, outputDir, quality)
	if err != nil {
		return "", err
	}

	// Rename the downloaded file to match the Spotify-derived filename.
	if spotifyTrackName != "" && spotifyArtistName != "" {
		newFilename := BuildExpectedFilename(spotifyTrackName, spotifyArtistName, spotifyAlbumName, spotifyAlbumArtist, spotifyReleaseDate, filenameFormat, includeTrackNumber, position, spotifyDiscNumber, useAlbumTrackNumber)
		newFilePath := filepath.Join(outputDir, newFilename)
		if err := os.Rename(filePath, newFilePath); err != nil {
			fmt.Printf("Warning: Failed to rename file: %v\n", err)
		} else {
			filePath = newFilePath
			fmt.Printf("Renamed to: %s\n", newFilename)
		}
	}

	fmt.Println("Embedding Spotify metadata...")

	coverPath := ""
	if spotifyCoverURL != "" {
		coverPath = filePath + ".cover.jpg"
		coverClient := NewCoverClient()
		if err := coverClient.DownloadCoverToPath(spotifyCoverURL, coverPath, embedMaxQualityCover); err != nil {
			fmt.Printf("Warning: Failed to download Spotify cover: %v\n", err)
			coverPath = ""
		} else {
			defer os.Remove(coverPath)
			fmt.Println("Spotify cover downloaded")
		}
	}

	trackNumberToEmbed := spotifyTrackNumber
	if trackNumberToEmbed == 0 {
		trackNumberToEmbed = 1
	}

	metadata := Metadata{
		Title:       spotifyTrackName,
		Artist:      spotifyArtistName,
		Album:       spotifyAlbumName,
		AlbumArtist: spotifyAlbumArtist,
		Date:        spotifyReleaseDate,
		TrackNumber: trackNumberToEmbed,
		TotalTracks: spotifyTotalTracks,
		DiscNumber:  spotifyDiscNumber,
		TotalDiscs:  spotifyTotalDiscs,
		URL:         spotifyURL,
		Copyright:   spotifyCopyright,
		Publisher:   spotifyPublisher,
		Description: "https://github.com/afkarxyz/SpotiFLAC",
	}

	if err := EmbedMetadata(filePath, metadata, coverPath); err != nil {
		fmt.Printf("Warning: Failed to embed metadata: %v\n", err)
	} else {
		fmt.Println("Metadata embedded successfully")
	}

	fmt.Println("Done")
	fmt.Println("✓ Downloaded successfully from Deezer")
	return filePath, nil
}
