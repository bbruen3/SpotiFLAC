package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"bufio"

	"spotiflac/backend"
)

func main() {

	app := NewApp()

	// CLI Flags
	setOutput := flag.String("set-output", "", "Set the default download directory")
	outputDir := flag.String("o", "", "Output directory for this download")
	folderFormat := flag.String("folder-format", "", "Optional folder structure template (e.g., '{artist}/{album}', '{album}'). Falls back to GUI config if not provided.")
	delay := flag.Duration("delay", 500*time.Millisecond, "Delay between downloads (e.g., 500ms, 1s)")
	concurrency := flag.Int("c", 3, "Number of concurrent downloads")
	service := flag.String("service", "tidal", "Download service: tidal, qobuz, amazon, deezer")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s [flags] [spotify-url]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nFlags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	services, svcErr := parseServices(*service)
	if svcErr != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid --service value: %v\n", svcErr)
		os.Exit(1)
	}

	args := flag.Args()

	if len(args) > 0 && args[0] == "conf" {
		runInteractiveConfig(app)
		return
	}

	// Handle Persistent Config Set
	if *setOutput != "" {
		if len(args) > 0 {
			// Check if a URL was also provided
			possibleURL := args[0]
			if strings.HasPrefix(possibleURL, "http") {
				fmt.Fprintln(os.Stderr, "Error: --set-output cannot be used together with a Spotify URL.")
				flag.Usage()
				os.Exit(1)
			}
		}
		handleSetOutput(*setOutput)
		return
	}

	// Handle CLI Download
	var validURLs []string
	if len(args) > 0 {
		for _, arg := range args {
			// Validate as an HTTPS Spotify URL
			if u, err := url.Parse(arg); err == nil && u.Scheme == "https" && strings.Contains(u.Host, "spotify.com") {
				validURLs = append(validURLs, arg)
			} else {
				// Inform the user that the argument was not recognized as a Spotify URL
				fmt.Fprintf(os.Stderr, "Warning: Unrecognized argument %q. Launching GUI instead (if no valid URLs found).\n", arg)
			}
		}

		if len(validURLs) > 0 {
			runCLI(app, validURLs, *outputDir, *folderFormat, *delay, *concurrency, services)
			return
		}
	}

	// Normal GUI Start (or error if headless)
	startGUI(app)
}

func handleSetOutput(path string) {
	// Normalize path (absolute)
	normalizedPath := backend.NormalizePath(path)
	absPath, err := filepath.Abs(normalizedPath)
	if err != nil {
		log.Fatalf("Failed to resolve absolute path: %v", err)
	}

	// Ensure the resolved path is either a directory or does not exist
	if info, err := os.Stat(absPath); err == nil {
		if !info.IsDir() {
			log.Fatalf("Path %s already exists and is not a directory", absPath)
		}
	} else if !os.IsNotExist(err) {
		log.Fatalf("Failed to stat path %s: %v", absPath, err)
	}

	// Create directory (idempotent)
	if err := os.MkdirAll(absPath, 0755); err != nil {
		log.Fatalf("Failed to create directory %s: %v", absPath, err)
	}

	if err := backend.SetConfiguration("downloadPath", absPath); err != nil {
		log.Fatalf("Failed to save configuration: %v", err)
	}

	fmt.Printf("Default download directory set to: %s\n", absPath)
}

func runCLI(app *App, spotifyURLs []string, outputDirOverride string, folderFormatOverride string, delay time.Duration, concurrency int, services []string) {
	// Manually manage the app lifecycle for CLI mode: in the normal Wails GUI flow,
	// Wails calls startup/shutdown for us and supplies the context; here we create
	// a signal-aware context that cancels on interrupt/termination signals and invoke
	// startup/shutdown ourselves.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	app.startup(ctx)
	defer app.shutdown(ctx)

	var tracksToDownload []DownloadRequest

	for _, spotifyURL := range spotifyURLs {
		// Robust URL validation (redundant if main checked, but good for safety)
		u, err := url.Parse(spotifyURL)
		if err != nil || u.Scheme == "" || u.Host == "" || !strings.Contains(u.Host, "spotify.com") {
			log.Printf("Error: invalid Spotify URL: %s. Skipping.", spotifyURL)
			continue
		}

		fmt.Printf("Analyzing Spotify URL: %s\n", spotifyURL)

		// Fetch metadata directly using backend
		data, err := backend.GetFilteredSpotifyData(ctx, spotifyURL, false, 0)
		if err != nil {
			log.Printf("Failed to fetch metadata for %s: %v", spotifyURL, err)
			continue
		}

		switch v := data.(type) {
		case backend.TrackResponse:
			fmt.Printf("Found Track: %s - %s\n", v.Track.Name, v.Track.Artists)
			req := mapTrackToDownloadRequest(v.Track)
			tracksToDownload = append(tracksToDownload, req)

		case *backend.AlbumResponsePayload:
			fmt.Printf("Found Album: %s - %s (%d tracks)\n", v.AlbumInfo.Name, v.AlbumInfo.Artists, len(v.TrackList))
			for _, t := range v.TrackList {
				req := mapAlbumTrackToDownloadRequest(t, v.AlbumInfo)
				tracksToDownload = append(tracksToDownload, req)
			}

		case backend.PlaylistResponsePayload:
			fmt.Printf("Found Playlist: %s (%d tracks)\n", v.PlaylistInfo.Owner.Name, len(v.TrackList))
			for _, t := range v.TrackList {
				req := mapAlbumTrackToDownloadRequest(t, backend.AlbumInfoMetadata{})
				tracksToDownload = append(tracksToDownload, req)
			}

		default:
			log.Printf("Unsupported Spotify content type via CLI for %s: %T", spotifyURL, v)
		}
	}

	if len(tracksToDownload) == 0 {
		fmt.Println("No tracks found to download.")
		return
	}

	fmt.Printf("Queued %d tracks for download (Concurrency: %d, Delay: %v)...\n", len(tracksToDownload), concurrency, delay)

	finalOutputDir := backend.GetDefaultMusicPath()
	if outputDirOverride != "" {
		normalizedOverride := backend.NormalizePath(outputDirOverride)
		absOverride, err := filepath.Abs(normalizedOverride)
		if err != nil {
			log.Fatalf("Failed to resolve absolute path for output directory override %q: %v", outputDirOverride, err)
		}
		finalOutputDir = absOverride
	}

	// Resolve folder template
	folderFormat := folderFormatOverride
	if folderFormat == "" {
		settings, err := app.LoadSettings()
		if err == nil && settings != nil {
			if template, ok := settings["folderTemplate"].(string); ok && template != "" {
				folderFormat = template
			}
		}
	}

	// Process downloads concurrently
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency) // Semaphore to limit concurrency
	var mu sync.Mutex

	successCount := 0
	failCount := 0

	total := len(tracksToDownload)

downloadLoop:
	for i, req := range tracksToDownload {
		// Check for cancellation before starting new downloads
		select {
		case <-ctx.Done():
			fmt.Println("\nDownload cancelled by user.")
			break downloadLoop
		default:
		}
		wg.Add(1)

		go func(idx int, r DownloadRequest) {
			sem <- struct{}{}        // Acquire token
			defer func() { <-sem }() // Release token
			defer wg.Done()

			// Simulating delay for politeness if needed across threads,
			// though less effective when strictly parallel, still helps stagger requests.
			clampedDelay := delay
			if clampedDelay < 0 {
				clampedDelay = 0
			}
			if clampedDelay > 0 {
				time.Sleep(clampedDelay)
			}

			// Generate the subfolder path
			subfolder := backend.BuildFolderPath(
				folderFormat,
				r.TrackName,
				r.ArtistName,
				r.AlbumName,
				r.AlbumArtist,
				r.ReleaseDate,
				"", // Playlist (not natively fetched mapped here unless we expand)
				r.Position,
				r.SpotifyDiscNumber,
			)

			if subfolder != "" {
				r.OutputDir = filepath.Join(finalOutputDir, subfolder)
			} else {
				r.OutputDir = finalOutputDir
			}

			if r.AudioFormat == "" {
				r.AudioFormat = "LOSSLESS"
			}

			// Serialize cascade calls to avoid concurrent toggling of global download state.
			mu.Lock()
			resp, err := app.DownloadTrackCascade(r, services)
			mu.Unlock()

			var resultMsg string
			var isSuccess bool

			if err != nil {
				resultMsg = fmt.Sprintf("[%d/%d] Failed: %s - %s (%v)", idx+1, total, r.TrackName, r.ArtistName, err)
				isSuccess = false
			} else if !resp.Success {
				resultMsg = fmt.Sprintf("[%d/%d] Failed: %s - %s (%s)", idx+1, total, r.TrackName, r.ArtistName, resp.Error)
				isSuccess = false
			} else {
				status := "Done"
				if resp.AlreadyExists {
					status = "Exists"
				}
				resultMsg = fmt.Sprintf("[%d/%d] %s: %s - %s", idx+1, total, status, r.TrackName, r.ArtistName)
				isSuccess = true
			}

			// Print immediately (might interleave slightly but acceptable for CLI)
			fmt.Println(resultMsg)

			mu.Lock()
			if isSuccess {
				successCount++
			} else {
				failCount++
			}
			mu.Unlock()

		}(i, req)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		app.WaitBackgroundTasks()
		close(done)
	}()

	select {
	case <-ctx.Done():
		os.Exit(1)
	case <-done:
		fmt.Printf("\nSummary: %d Success, %d Failed. Output dir: %s\n", successCount, failCount, finalOutputDir)
		if failCount > 0 && successCount == 0 {
			os.Exit(1)
		} else if failCount > 0 {
			os.Exit(2)
		}
	}
}

func mapTrackToDownloadRequest(t backend.TrackMetadata) DownloadRequest {
	return DownloadRequest{
		SpotifyID:          t.SpotifyID,
		ISRC:               t.ISRC,
		TrackName:          t.Name,
		ArtistName:         t.Artists,
		AlbumName:          t.AlbumName,
		AlbumArtist:        t.AlbumArtist,
		ReleaseDate:        t.ReleaseDate,
		CoverURL:           t.Images,
		TrackNumber:        true,
		Position:           t.TrackNumber,
		SpotifyTrackNumber: t.TrackNumber,
		SpotifyDiscNumber:  t.DiscNumber,
		SpotifyTotalTracks: t.TotalTracks,
		SpotifyTotalDiscs:  t.TotalDiscs,
		Copyright:          t.Copyright,
		Publisher:          t.Publisher,
		Duration:           t.DurationMS,
		EmbedLyrics:        true,
	}
}

func mapAlbumTrackToDownloadRequest(t backend.AlbumTrackMetadata, albumInfo backend.AlbumInfoMetadata) DownloadRequest {
	req := DownloadRequest{
		SpotifyID:          t.SpotifyID,
		ISRC:               t.ISRC,
		TrackName:          t.Name,
		ArtistName:         t.Artists,
		AlbumName:          t.AlbumName,
		AlbumArtist:        t.AlbumArtist,
		ReleaseDate:        t.ReleaseDate,
		CoverURL:           t.Images,
		TrackNumber:        true,
		Position:           t.TrackNumber,
		SpotifyTrackNumber: t.TrackNumber,
		SpotifyDiscNumber:  t.DiscNumber,
		SpotifyTotalTracks: t.TotalTracks,
		SpotifyTotalDiscs:  t.TotalDiscs,
		Duration:           t.DurationMS,
		EmbedLyrics:        true,
	}

	// Fallback to album info if track info is missing some details
	if req.AlbumName == "" {
		req.AlbumName = albumInfo.Name
	}
	if req.ReleaseDate == "" {
		req.ReleaseDate = albumInfo.ReleaseDate
	}

	// Known limitation: for playlist items, AlbumInfoMetadata may be incomplete or empty.
	// We check if we can infer from the track metadata itself using fields found in AlbumTrackMetadata if available.
	if req.AlbumName == "" && t.AlbumName != "" {
		req.AlbumName = t.AlbumName
	}
	if req.ReleaseDate == "" && t.ReleaseDate != "" {
		req.ReleaseDate = t.ReleaseDate
	}

	// At this point, playlist items are expected to have AlbumName and ReleaseDate populated
	// via AlbumTrackMetadata, with AlbumInfoMetadata used as a fallback when available.
	// If new cases arise where playlist tracks legitimately lack album data, extend the
	// fallback logic above to cover those scenarios.

	return req
}

func runInteractiveConfig(app *App) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("--- SpotiFLAC Interactive Configuration ---")

	settings, err := app.LoadSettings()
	if err != nil {
		fmt.Printf("Error loading settings: %v\n", err)
		return
	}
	if settings == nil {
		settings = make(map[string]interface{})
	}

	// Option 1: Default Download Path
	currentPath := ""
	if p, ok := settings["downloadPath"].(string); ok {
		currentPath = p
	}
	if currentPath == "" {
		currentPath = backend.GetDefaultMusicPath()
	}
	fmt.Printf("1. Default Download Directory [%s]: ", currentPath)
	pathInput, _ := reader.ReadString('\n')
	pathInput = strings.TrimSpace(pathInput)

	if pathInput != "" {
		normalizedPath := backend.NormalizePath(pathInput)
		absPath, err := filepath.Abs(normalizedPath)
		if err == nil {
			settings["downloadPath"] = absPath
			currentPath = absPath
		}
	}

	// Option 2: Folder Formatting
	currentPreset := "none"
	if p, ok := settings["folderPreset"].(string); ok && p != "" {
		currentPreset = p
	}
	currentTemplate := ""
	if t, ok := settings["folderTemplate"].(string); ok {
		currentTemplate = t
	}

	fmt.Printf("\n2. Folder Preset [%s]\n", currentPreset)
	fmt.Println("   1) none (No Subfolder)")
	fmt.Println("   2) artist ({artist})")
	fmt.Println("   3) album ({album})")
	fmt.Println("   4) artist-album ({artist}/{album})")
	fmt.Println("   5) custom")
	fmt.Print("Select option (1-5) or press Enter to keep current: ")
	
	presetInput, _ := reader.ReadString('\n')
	presetInput = strings.TrimSpace(presetInput)

	switch presetInput {
	case "1":
		settings["folderPreset"] = "none"
		settings["folderTemplate"] = ""
	case "2":
		settings["folderPreset"] = "artist"
		settings["folderTemplate"] = "{artist}"
	case "3":
		settings["folderPreset"] = "album"
		settings["folderTemplate"] = "{album}"
	case "4":
		settings["folderPreset"] = "artist-album"
		settings["folderTemplate"] = "{artist}/{album}"
	case "5":
		settings["folderPreset"] = "custom"
		fmt.Printf("Enter custom folder format (e.g. {artist}/{album}) [%s]: ", currentTemplate)
		customInput, _ := reader.ReadString('\n')
		customInput = strings.TrimSpace(customInput)
		if customInput != "" {
			settings["folderTemplate"] = customInput
		}
	}

	err = app.SaveSettings(settings)
	if err != nil {
		fmt.Printf("\nFailed to save settings to config.json: %v\n", err)
		return
	}
	
	// Also sync downloadPath to history DB
	if p, ok := settings["downloadPath"].(string); ok && p != "" {
		backend.SetConfiguration("downloadPath", p)
	}

	fmt.Println("\nConfiguration saved successfully!")
}

// parseServices parses the --service flag value into an ordered slice of service
// names. "auto" expands to the full default cascade order. A comma-separated list
// (e.g. "deezer,tidal") is tried left-to-right. A single name is also valid.
func parseServices(raw string) ([]string, error) {
	if raw == "auto" {
		raw = "tidal,deezer,amazon,qobuz"
	}
	known := map[string]bool{"tidal": true, "qobuz": true, "amazon": true, "deezer": true}
	parts := strings.Split(raw, ",")
	seen := map[string]bool{}
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if !known[p] {
			return nil, fmt.Errorf("unknown service %q — valid values: tidal, qobuz, amazon, deezer, auto", p)
		}
		if seen[p] {
			return nil, fmt.Errorf("duplicate service %q in list", p)
		}
		seen[p] = true
		result = append(result, p)
	}
	return result, nil
}
