package wakanda_gssa_plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/fubarov/gssa-sdk"
	"github.com/fubarov/wakanda-gssa-plugin/internal/utils/regexps"

	"github.com/PuerkitoBio/goquery"
	torrentinfo "github.com/sweetbbak/torrent-info"
)

type Wakanda struct {
	isConfigured bool
	logger       gssa_sdk.Logger
	settings     *Settings
	jar          *cookiejar.Jar
	client       *http.Client
}

type Settings struct {
	AppID          string          `json:"app_id"`
	AppName        string          `json:"app_name"`
	AppShortName   string          `json:"app_short_name"`
	AppDescription string          `json:"app_description"`
	Tracker        TrackerSettings `json:"tracker"`
	Proxy          ProxySettings   `json:"proxy"`
}

type TrackerSettings struct {
	Url       string `json:"url"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	UserID    string `json:"user_id"`
	UserAgent string `json:"user_agent"`
}

type ProxySettings struct {
	Enabled bool   `json:"enabled"`
	Url     string `json:"url"`
}

func (p ProxySettings) IsEnabled() bool {
	return p.Enabled
}

func init() {
	gssa_sdk.RegisterPlugin(&Wakanda{
		isConfigured: false,
		settings:     &Settings{},
	})
}

func (w *Wakanda) Init(rawConfig json.RawMessage, logger gssa_sdk.Logger) {
	w.logger = logger
	w.isConfigured = true
	w.jar, _ = cookiejar.New(nil)

	var transport = &http.Transport{}
	if w.settings.Proxy.IsEnabled() {
		proxyURL, err := url.Parse(w.settings.Proxy.Url)
		if err != nil {
			w.logger.Error(err)
		} else {
			transport = &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			}
		}
	}

	w.client = &http.Client{
		Jar:       w.jar,
		Transport: transport,
	}

	if err := json.Unmarshal(rawConfig, w.settings); err != nil {
		w.isConfigured = false
		w.logger.Error(fmt.Sprintf("Failed to parse plugin config: %v", err))
	}

	if w.settings.AppID == "" {
		w.isConfigured = false
		w.logger.Error("Plugin config is invalid: missing app_id")
	}

	if !w.isConfigured {
		return
	}

	_ = w.login()
}

func (w *Wakanda) login() error {
	form := url.Values{}
	form.Set("username", w.settings.Tracker.Username)
	form.Set("password", w.settings.Tracker.Password)

	req, _ := http.NewRequest("POST", w.settings.Tracker.Url+"/takelogin.php", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", w.settings.Tracker.UserAgent)

	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			return
		}
	}(resp.Body)

	var body []byte
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if w.isConfirmedLogin(body) {
		return nil
	}

	return fmt.Errorf("login failed")
}

func (w *Wakanda) request(url string, isDownload, isRetry bool) (response string) {
	req, err := http.NewRequest("GET", w.settings.Tracker.Url+url, nil)
	if err != nil {
		w.logger.Error(err)
		return
	}
	resp, err := w.client.Do(req)
	if err != nil {
		w.logger.Error(err)
		return
	}
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			w.logger.Error(err)
			return
		}
	}(resp.Body)

	var body []byte
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		w.logger.Error(err)
		return
	}

	if !w.isConfirmedLogin(body) && !isDownload && !isRetry {
		w.logger.Error("Not logged in, retrying!")

		_ = w.login()
		return w.request(url, isDownload, true)
	}

	response = string(body)
	return
}

func (w *Wakanda) isConfirmedLogin(body []byte) bool {
	return strings.Contains(string(body), "userdetails.php?id="+w.settings.Tracker.UserID)
}

func (w *Wakanda) pluckEntities(html, season, episode string) (entities []StreamItem) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		w.logger.Error("Parse error: ", err)
	}

	var wg sync.WaitGroup
	var mutex sync.Mutex

	doc.Find(`table.test[width="720"][cellpadding="5"]`).Each(func(i int, s *goquery.Selection) {
		s.Find("tr").First().NextAll().Each(func(j int, tr *goquery.Selection) {
			wg.Add(1)
			go w.pluckEntity(&wg, &mutex, *tr, &entities, season, episode)
		})
	})

	wg.Wait()

	return
}

func (w *Wakanda) pluckEntity(wg *sync.WaitGroup, mutex *sync.Mutex, tr goquery.Selection, entities *[]StreamItem, season, episode string) {
	defer wg.Done()
	var err error
	// Extract from the torrent link
	href, exists := tr.Find(`a[href^="/download.php/"]`).First().Attr("href")
	if !exists {
		w.logger.Error("href not found")
		return
	}

	parts := strings.Split(href, "/")
	if len(parts) < 3 {
		w.logger.Error("href not valid")
		return
	}

	title := parts[3] // the rest of the URL after the ID

	bgAudio := regexps.BgAudio.MatchString(title)

	match := regexps.Resolution.FindStringSubmatch(href)
	resolution := "480p"
	if len(match) > 1 {
		resolution = match[1]
	}

	// Extract seeders count
	entitySeeders := tr.Find(`td.tddownloaded center font a`).First().Find("b").Text()
	seedInt, _ := strconv.Atoi(strings.TrimSpace(entitySeeders))

	// Extract bg audio flag
	if !bgAudio {
		bgAudio = tr.Find(`img[src*="flag_bgaudio"]`).First().Length() > 0
	}

	var decodedTitle string
	decodedTitle, err = url.QueryUnescape(title)
	if err != nil {
		w.logger.Error(err)
		return
	}

	var torrentBuffer []byte
	torrentBuffer = []byte(w.request(href, true, false))
	if len(torrentBuffer) == 0 {
		w.logger.Error("Could not download torrent file.")
		return
	}

	infoHash, sizeGB, parsedFileIdx, fileName, err := ParseTorrentBytes(torrentBuffer, season, episode)
	if err != nil {
		w.logger.Error("failed to parse torrent:", err)
		return
	}

	var fileIdx int
	if parsedFileIdx != -1 {
		fileIdx = parsedFileIdx
	}

	var behaviorHints gssa_sdk.BehaviorHints

	if season != "" && episode != "" {
		if fileName != "" {
			if !bgAudio {
				bgAudio = regexps.BgAudio.MatchString(fileName)
			}
			decodedTitle = fileName
			behaviorHints = gssa_sdk.BehaviorHints{
				BingeGroup: strings.ToLower(w.settings.AppShortName) + "|" + infoHash,
				Filename:   fileName,
			}
		} else {
			return
		}
	}

	mutex.Lock()
	*entities = append(*entities, StreamItem{
		Title:         decodedTitle,
		InfoHash:      infoHash,
		Size:          sizeGB,
		Seeders:       seedInt,
		BgAudio:       bgAudio,
		Resolution:    resolution,
		FileIdx:       &fileIdx,
		BehaviorHints: &behaviorHints,
	})
	mutex.Unlock()
}

func ParseTorrentBytes(buf []byte, season, episode string) (string, float64, int, string, error) {
	t, err := torrentinfo.Parse(bytes.NewReader(buf))
	if err != nil {
		return "", 0, -1, "", err
	}

	infoHash := t.InfoHash

	var total int64
	for _, f := range t.Files {
		total += f.Length
	}
	sizeGB := float64(total) / (1024 * 1024 * 1024)

	var videoExt = map[string]bool{
		".mkv": true,
		".mp4": true,
		".avi": true,
		".mov": true,
	}

	needle := strings.ToLower(season + episode)
	for fIndex, f := range t.Files {
		for _, s := range f.Path {
			ext := strings.ToLower(filepath.Ext(s))
			if !videoExt[ext] {
				continue
			}
			lowerFile := strings.ToLower(s)

			if strings.Contains(lowerFile, needle) {
				fileName := s
				fileIdx := fIndex
				return infoHash, sizeGB, fileIdx, fileName, nil
			}
		}
	}

	return infoHash, sizeGB, -1, "", nil
}

func (w *Wakanda) SearchMoviesByImdbID(target gssa_sdk.TargetMovie) []gssa_sdk.Stream {
	if !w.isConfigured {
		w.logger.Error("Plugin is not configured")
		return []gssa_sdk.Stream{}
	}

	if target.ImdbID == "" {
		w.logger.Error("Invalid target: missing imdb_id")
		return []gssa_sdk.Stream{}
	}

	return w.searchMovies(target.ImdbID)
}

func (w *Wakanda) SearchSeriesByImdbID(target gssa_sdk.TargetSeries) (streams []gssa_sdk.Stream) {
	if !w.isConfigured {
		w.logger.Error("Plugin is not configured")
		return streams
	}
	if target.ImdbID == "" {
		w.logger.Error("Invalid target: missing imdb_id")
		return streams
	}
	if target.Season == 0 {
		w.logger.Error("Invalid target: missing season")
		return streams
	}
	if target.Episode == 0 {
		w.logger.Error("Invalid target: missing episode")
		return streams
	}
	return w.searchSeries(target, true)
}

func (w *Wakanda) searchSeries(target gssa_sdk.TargetSeries, searchEpisodes bool) []gssa_sdk.Stream {
	categoryPath := "tv?t=tv"
	searchElements := []string{target.ImdbID}
	season := ""
	episode := ""

	if target.Season > 0 {
		prefix := ""
		if target.Episode < 10 {
			prefix = "0"
		}
		season = prefix + strconv.Itoa(target.Season)
		searchElements = append(searchElements, season)
	}

	if target.Episode > 0 && searchEpisodes {
		prefix := ""
		if target.Episode < 10 {
			prefix = "0"
		}
		episode = prefix + strconv.Itoa(target.Episode)
		searchElements = append(searchElements, episode)
	}

	searchString := strings.Join(searchElements, "+")

	results := w.search(searchString, categoryPath, season, episode)
	if len(results) == 0 && searchEpisodes {
		return w.searchSeries(target, false)
	}

	return results
}

func (w *Wakanda) searchMovies(searchString string) []gssa_sdk.Stream {
	categoryPath := "movies?t=movie"

	return w.search(searchString, categoryPath, "", "")
}

func (w *Wakanda) search(searchString string, categoryPath string, season, episode string) (streams []gssa_sdk.Stream) {
	var html string
	path := fmt.Sprintf("/catalogs/%s&comb=yes&search=%s&field=descr&sort=9&type=desc", categoryPath, searchString)

	w.logger.Debug("Searching for: ", searchString)
	w.logger.Debug("Search path: ", path)

	html = w.request(path, false, false)

	if html == "" {
		w.logger.Debug("No results found!")
		return
	}

	streamItems := w.pluckEntities(html, season, episode)

	for _, movie := range streamItems {
		bgAudio := ""
		if movie.BgAudio {
			bgAudio = "🇧🇬🎧"
		}

		description := fmt.Sprintf(
			"%s\n📺  %s\n👤  %d 💾  %.2f GB\n%s",
			movie.Title,
			movie.Resolution,
			movie.Seeders,
			movie.Size,
			bgAudio,
		)
		streams = append(streams, gssa_sdk.Stream{
			InfoHash:      movie.InfoHash,
			Name:          w.settings.AppShortName + "\n" + movie.Resolution,
			Description:   description,
			FileIdx:       movie.FileIdx,
			BehaviorHints: movie.BehaviorHints,
		})
	}
	SortStreamsByResolution(streams)

	return
}

func (w *Wakanda) GenerateManifest() gssa_sdk.Manifest {
	return gssa_sdk.Manifest{
		ID:            w.settings.AppID,
		Version:       "1.0.1",
		Name:          w.settings.AppShortName,
		Description:   w.settings.AppDescription,
		Resources:     []interface{}{"stream"},
		Types:         []string{"movie", "series"},
		Catalogs:      []string{},
		IDPrefixes:    []string{"tt"},
		BehaviorHints: &gssa_sdk.BehaviorHints{P2P: true},
	}
}

func SortStreamsByResolution(streams []gssa_sdk.Stream) {
	resPriority := map[string]int{
		"8K":    6,
		"4K":    5,
		"2160P": 5, // treat 4 K and 2160p the same
		"1080P": 4,
		"720P":  3,
		"480P":  2,
	}

	sort.Slice(streams, func(i, j int) bool {
		// extract resolution from Name
		resI := "480p" // default if no match
		if m := regexps.Resolution.FindStringSubmatch(streams[i].Name); len(m) > 0 {
			resI = m[0]
		}

		resJ := "480p"
		if m := regexps.Resolution.FindStringSubmatch(streams[j].Name); len(m) > 0 {
			resJ = m[0]
		}

		// get priority (convert to uppercase for safety)
		priI := resPriority[strings.ToUpper(resI)]
		priJ := resPriority[strings.ToUpper(resJ)]

		return priI > priJ // descending order
	})
}
