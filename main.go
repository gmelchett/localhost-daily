package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	urlHandle "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/OpenPeeDeeP/xdg"
	"github.com/dustin/go-humanize"
	"github.com/mmcdole/gofeed"
	"github.com/nathan-osman/go-sunrise"
	"github.com/pelletier/go-toml"
	"golang.org/x/net/html"
)

//go:embed templates/index.html
var tmplSrc string

type Config struct {
	Title           string         `toml:"title"`
	Feeds           []string       `toml:"feeds"`
	Buttons         []ButtonConfig `toml:"buttons"`
	MaxDesc         int            `toml:"max_description_chars"`
	ServerAddr      string         `toml:"server_addr"`
	Lat             float64        `toml:"lat"`
	Lon             float64        `toml:"lon"`
	MaxAge          int            `toml:"max_age_in_days"`
	Columns         int            `toml:"columns"`
	PingHost        string         `toml:"ping_host"`
	ElectricityArea string         `toml:"electricity_area"`
	CurrencyFrom1   string         `toml:"currency_from_1"`
	CurrencyTo1     string         `toml:"currency_to_1"`
	CurrencyFrom2   string         `toml:"currency_from_2"`
	CurrencyTo2     string         `toml:"currency_to_2"`
	MemGaugeColor   string         `toml:"memory_gauge_color"`
	DiskGaugeColor  string         `toml:"disk_gauge_color"`
	DiskMountPoint  string         `toml:"disk_mount_point"`
}

type ButtonConfig struct {
	Icon  string `toml:"icon"`
	Link  string `toml:"link"`
	Title string `toml:"title"`
	Sub   string `toml:"sub"`
}

type FeedItem struct {
	Title       string
	Link        string
	Published   string
	Description string
	Image       string
	Source      string
	Layout      string // "side" or "stacked"
	imageWidth  int
	imageHeight int
}

type PageData struct {
	GeneratedAt             time.Time
	DateString              string
	Title                   string
	Fortune                 string
	TodayTemp               string
	TodaySymbol             string
	TomorrowTemp            string
	TomorrowSymbol          string
	Items                   [][]FeedItem // columns of items
	Buttons                 []ButtonConfig
	PublicIP                string
	PingResult              string
	Uptime                  string
	SunRise                 string
	SunSet                  string
	MemUsage                float64
	MemArc                  string
	DiskUsage               float64
	DiskArc                 string
	ElectricityPriceCurrent string
	ElectricityPriceMinMax  string
	ExchangeRate1           string
	ExchangeRate2           string
	MemGaugeColor           string
	DiskGaugeColor          string
}

var (
	cfg     Config
	tmpl    *template.Template
	fp      = gofeed.NewParser()
	xdgPath = xdg.New("gmelchett", "localhost-daily")
)

func main() {
	rand.Seed(time.Now().UnixNano())

	configPath := flag.String("config", "", "Path to config.toml")
	outputPath := flag.String("output", ".", "Output directory")
	flag.Parse()

	if *configPath == "" {
		log.Fatal("Missing -config argument")
	}

	if err := loadConfig(*configPath); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	createDir(xdgPath.CacheHome())

	var err error
	tmpl, err = template.New("index").Parse(tmplSrc)
	if err != nil {
		log.Fatalf("Template parse: %v", err)
	}

	items := fetchAllFeeds(cfg.Feeds)
	page := buildPageData(items)

	absPath, err := filepath.Abs(*outputPath)
	if err != nil {
		log.Fatalf("Error converting to absolute path: %v", err)
	}

	err = os.MkdirAll(absPath, 0755)
	if err != nil {
		log.Fatalf("Error creating output directory: %v", err)
	}

	f, err := os.Create(filepath.Join(*outputPath, "index.html"))
	if err != nil {
		log.Fatalf("Cannot create index.html file: %v", err)
	}
	defer f.Close()

	if err := tmpl.Execute(f, page); err != nil {
		log.Fatalf("Template exec error: %v", err)
	}
}

func fetchAllFeeds(urls []string) []FeedItem {
	results := make([]FeedItem, 0, 200)
	for _, u := range urls {
		it, err := fetchFeed(u)
		if err != nil {
			log.Printf("Fetch feed %s failed: %v", u, err)
			continue
		}
		results = append(results, it...)
	}
	rand.Shuffle(len(results), func(i, j int) { results[i], results[j] = results[j], results[i] })
	return results
}

func loadConfig(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	cfg = Config{
		MaxDesc:    300,
		ServerAddr: ":8080",
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return err
	}
	if cfg.MaxDesc <= 0 {
		cfg.MaxDesc = 300
	}
	return nil
}

func getImageResolution(imageURL string) (int, int) {
	if len(imageURL) == 0 {
		return 0, 0
	}

	hash := sha256.Sum256([]byte(imageURL))
	cacheImageName := filepath.Join(xdgPath.CacheHome(), hex.EncodeToString(hash[:]))

	if f, err := os.Open(cacheImageName); err == nil {
		defer f.Close()
		if imgConfig, _, err := image.DecodeConfig(f); err == nil {
			return imgConfig.Width, imgConfig.Height
		}
	}

	resp, err := http.Get(imageURL)
	if err != nil {
		return 0, 0
	}
	defer resp.Body.Close()

	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(resp.Body)
	if err != nil {
		return 0, 0
	}

	imgConfig, _, err := image.DecodeConfig(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return 0, 0
	}
	os.WriteFile(cacheImageName, buf.Bytes(), 0o644)

	return imgConfig.Width, imgConfig.Height
}

func fetchFeed(url string) ([]FeedItem, error) {
	feed, err := fp.ParseURL(url)
	if err != nil {
		return nil, err
	}

	u, _ := urlHandle.Parse(url)

	out := make([]FeedItem, 0, len(feed.Items))

	oldest := time.Now().AddDate(0, 0, -cfg.MaxAge)

	for _, it := range feed.Items {
		fi := FeedItem{
			Title:     strings.TrimSpace(it.Title),
			Link:      it.Link,
			Source:    feed.Title,
			Published: humanize.Time(time.Now()),
			Layout:    "side",
		}

		if len(fi.Link) > 0 && fi.Link[0] == '/' && u != nil {
			fi.Link = fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, fi.Link)
		}

		if it.PublishedParsed != nil {

			if (*it.PublishedParsed).Before(oldest) {
				continue
			}

			fi.Published = humanize.Time(*it.PublishedParsed)
		}

		if it.Image != nil && it.Image.URL != "" {
			fi.Image = it.Image.URL
			if fi.Image[0] == '/' {
				if u != nil {
					fi.Image = fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, fi.Image)
				}
			}
		}
		if fi.Image == "" {
			if m, ok := it.Extensions["media"]; ok {
				if ext, ok := m["thumbnail"]; ok && len(ext) > 0 {
					if v, ok := ext[0].Attrs["url"]; ok {
						fi.Image = v
					}
				}
			}
		}
		if fi.Image == "" && len(it.Enclosures) > 0 {
			for _, e := range it.Enclosures {
				if strings.HasPrefix(e.Type, "image") && e.URL != "" {
					fi.Image = e.URL
				}
			}
		}
		if fi.Image == "" && it.Description != "" {
			if img := extractImgSrc(it.Description); img != "" {
				fi.Image = img
			}
		}

		fi.imageWidth, fi.imageHeight = getImageResolution(fi.Image)

		if fi.imageWidth < 200 || fi.imageHeight < 200 {
			fi.Image = ""
		}

		if fi.imageWidth > 600 && rand.Intn(2) == 0 {
			fi.Layout = "stacked"
		}

		desc := stripTags(it.Description)
		if desc == "" && it.Content != "" {
			desc = stripTags(it.Content)
		}
		fi.Description = truncateString(desc, cfg.MaxDesc)

		out = append(out, fi)
	}
	return out, nil
}

func getFortune(tries int, maxLen int) string {
	var best string
	for i := 0; i < tries; i++ {
		out, err := exec.Command("fortune", "-s").Output()
		if err != nil {
			return "No cookie today — fortune program not found."
		}
		f := strings.TrimSpace(strings.ReplaceAll(string(out), "\n", " "))
		if f == "" {
			continue
		}
		if len(f) <= maxLen {
			return f
		}
		if best == "" || len(f) < len(best) {
			best = f
		}
	}
	if best != "" {
		return best
	}
	// Have a default
	return "When the speaker and he to whom he is speaks do not understand, that is metaphysics. -- Voltaire"
}

func getExchangeRate(from, to string) (float64, error) {

	type response struct {
		Amount float64            `json:"amount"`
		Base   string             `json:"base"`
		Date   string             `json:"date"`
		Rates  map[string]float64 `json:"rates"`
	}

	url := fmt.Sprintf(
		"https://api.frankfurter.app/latest?amount=1&from=%s&to=%s",
		from, to,
	)

	resp, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	var data response
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return 0, err
	}

	rate, ok := data.Rates[to]
	if !ok {
		return 0, fmt.Errorf("rate for %s not found in response", to)
	}
	return rate, nil
}

func fetchCurrentElectricityPrice(area string) ([]float64, error) {
	if area == "" {
		return nil, fmt.Errorf("Bad area")
	}

	type PriceEntry struct {
		SEKPerKWh float64 `json:"SEK_per_kWh"`
		EURPerKWh float64 `json:"EUR_per_kWh"`
		EXR       float64 `json:"EXR"`
		TimeStart string  `json:"time_start"`
		TimeEnd   string  `json:"time_end"`
	}

	currentTime := time.Now()
	url := fmt.Sprintf("https://www.elprisetjustnu.se/api/v1/prices/%d/%02d-%02d_%s.json",
		currentTime.Year(),
		int(currentTime.Month()),
		currentTime.Day(), area)

	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch price data: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %v", err)
	}

	var priceEntries []PriceEntry
	err = json.Unmarshal(body, &priceEntries)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %v", err)
	}

	currPrice := 0.0
	minPrice := 1000.0
	maxPrice := -1000.0

	for _, entry := range priceEntries {
		startTime, err := time.Parse(time.RFC3339, entry.TimeStart)
		if err != nil {
			continue
		}
		endTime, err := time.Parse(time.RFC3339, entry.TimeEnd)
		if err != nil {
			continue
		}

		if currentTime.After(startTime) && currentTime.Before(endTime) {
			currPrice = entry.SEKPerKWh
		}
		if entry.SEKPerKWh < minPrice {
			minPrice = entry.SEKPerKWh
		}
		if entry.SEKPerKWh > maxPrice {
			maxPrice = entry.SEKPerKWh
		}
	}

	return []float64{currPrice, minPrice, maxPrice}, nil
}

func getUptimeStartDate() string {

	content, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return ""
	}

	uptimeFields := strings.Fields(string(content))
	if len(uptimeFields) == 0 {
		return ""
	}

	uptimeSeconds, err := strconv.ParseFloat(uptimeFields[0], 64)
	if err != nil {
		return ""
	}

	bootTime := time.Now().Add(-time.Duration(uptimeSeconds * float64(time.Second)))

	return fmt.Sprintf("Up since %s", bootTime.Format("Monday, January 2, 2006"))
}

var weatherIconMapping = map[int]string{
	1:  "wi-day-sunny",     // Clear sky
	2:  "wi-day-cloudy",    // Nearly clear sky
	3:  "wi-cloudy",        // Variable cloudiness
	4:  "wi-day-haze",      // Halfclear sky
	5:  "wi-cloud",         // Cloudy sky
	6:  "wi-cloudy",        // Overcast
	7:  "wi-fog",           // Fog
	8:  "wi-rain",          // Light rain showers
	9:  "wi-showers",       // Rain showers
	10: "wi-rain-wind",     // Heavy rain showers
	11: "wi-thunderstorm",  // Thunderstorm
	12: "wi-sleet",         // Light sleet showers
	13: "wi-sleet",         // Sleet showers
	14: "wi-storm-showers", // Heavy sleet showers
	15: "wi-snow",          // Light snow showers
	16: "wi-snow",          // Snow showers
	17: "wi-snow-wind",     // Heavy snow showers
	18: "wi-rain",          // Light rain
	19: "wi-rain",          // Rain
	20: "wi-rain-wind",     // Heavy rain
	21: "wi-lightning",     // Thunder
	22: "wi-sleet",         // Light sleet
	23: "wi-sleet",         // Sleet
	24: "wi-storm-showers", // Heavy sleet
	25: "wi-snow",          // Light snowfall
	26: "wi-snow",          // Snowfall
	27: "wi-snow-wind",     // Heavy snowfall
}

func fetchWeather(lat, lon float64) (temp []float64, symbols []string) {
	type Parameter struct {
		Name   string    `json:"name"`
		Values []float64 `json:"values"`
	}

	type TimeSeries struct {
		ValidTime  string      `json:"validTime"`
		Parameters []Parameter `json:"parameters"`
	}

	type SMHIForecast struct {
		TimeSeries []TimeSeries `json:"timeSeries"`
	}

	url := fmt.Sprintf("https://opendata-download-metfcst.smhi.se/api/category/pmp3g/version/2/geotype/point/lon/%.6f/lat/%.6f/data.json", lon, lat)

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	var forecast SMHIForecast
	err = json.Unmarshal(body, &forecast)
	if err != nil {
		return
	}

	findParameterValue := func(series TimeSeries, paramName string) float64 {
		for _, param := range series.Parameters {
			if param.Name == paramName && len(param.Values) > 0 {
				return param.Values[0]
			}
		}
		return 0
	}

	for _, series := range forecast.TimeSeries[:48] {
		validTime, err := time.Parse(time.RFC3339, series.ValidTime)

		if err != nil || validTime.Minute() != 0 || validTime.Hour() != 12 {
			continue
		}

		temp = append(temp, findParameterValue(series, "t"))

		symbols = append(symbols, weatherIconMapping[int(findParameterValue(series, "Wsymb2"))])
		if len(symbols) == 2 {
			break
		}
	}

	return
}

// arcPath returns an SVG arc path for a semicircular gauge
// usage should be 0..100 (percent). The semicircle is drawn
// from the leftmost point (10,50) across the top toward the right (90,50).
func arcPath(usage float64) string {
	// clamp
	if usage <= 0 {
		return "" // nothing to draw for 0%
	}
	if usage > 100 {
		usage = 100
	}

	// theta runs from 0..pi (0 => leftmost, pi => rightmost)
	theta := math.Pi * usage / 100.0
	r := 40.0
	cx, cy := 50.0, 50.0

	// compute end point for given theta:
	// x = cx - r*cos(theta)
	// y = cy - r*sin(theta)
	x := cx - r*math.Cos(theta)
	y := cy - r*math.Sin(theta)

	// large-arc-flag: 0 because theta never exceeds pi (<=180deg)
	// sweep-flag: 1 to draw in increasing-theta direction (left -> top -> right)
	return fmt.Sprintf("M10,50 A40,40 0 0,1 %.1f,%.1f", x, y)
}

func getMemUsage() float64 {
	var mem syscall.Sysinfo_t
	syscall.Sysinfo(&mem)
	total := float64(mem.Totalram)
	free := float64(mem.Freeram)
	return 100 * (1 - free/total)
}

func getDiskUsage() float64 {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(cfg.DiskMountPoint, &fs); err == nil {
		totalDisk := float64(fs.Blocks) * float64(fs.Bsize)
		freeDisk := float64(fs.Bavail) * float64(fs.Bsize)
		return 100 * (1 - freeDisk/totalDisk)
	}
	return 0.0
}

func pingHost(host string) (string, error) {

	re := regexp.MustCompile(`time=(\d+\.\d+)\s*ms`)

	pingCmd := exec.Command("ping", "-c", "11", host)
	pingOut, err := pingCmd.CombinedOutput()
	if err != nil {
		return "", err
	}

	outputStr := string(pingOut)

	matches := re.FindAllStringSubmatch(outputStr, -1)

	var latencies []float64
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}

		latency, err := strconv.ParseFloat(match[1], 64)
		if err == nil {
			latencies = append(latencies, latency)
		}
	}

	// Calculate median
	sort.Float64s(latencies)

	n := len(latencies)
	if n == 0 {
		return "", fmt.Errorf("no latencies found in ping output")
	}
	if n%2 == 0 {
		return fmt.Sprintf("%.1f ms", (latencies[n/2-1]+latencies[n/2])/2), nil
	}
	return fmt.Sprintf("%.1f ms", latencies[n/2]), nil
}

func balanceColumns(columns [][]FeedItem, maxHeightDiff int) [][]FeedItem {
	heights := make([]int, len(columns))

	for i := range columns {
		for _, item := range columns[i] {
			heights[i] += getItemHeight(item)
		}
	}

	// Redistribute items while maintaining height balance
	for {
		maxHeight := slices.Max(heights)
		minHeight := slices.Min(heights)

		if maxHeight-minHeight <= maxHeightDiff {
			break // Columns are balanced enough
		}

		// Find the tallest and shortest column indices
		tallestIdx := slices.Index(heights, maxHeight)
		shortestIdx := slices.Index(heights, minHeight)

		// Move the last item from the tallest column to the shortest
		if len(columns[tallestIdx]) > 0 {
			lastItem := columns[tallestIdx][len(columns[tallestIdx])-1]
			columns[tallestIdx] = columns[tallestIdx][:len(columns[tallestIdx])-1]
			columns[shortestIdx] = append(columns[shortestIdx], lastItem)

			// Recalculate heights
			heights[tallestIdx] -= getItemHeight(lastItem)
			heights[shortestIdx] += getItemHeight(lastItem)
		}
	}

	return columns
}

func getItemHeight(item FeedItem) int {
	if item.Layout == "stacked" {
		return 4
	} else if item.Image != "" {
		return 2
	}
	return 1
}

func buildPageData(items []FeedItem) PageData {
	now := time.Now()
	cols := cfg.Columns

	columns := make([][]FeedItem, cols)
	for i, it := range items {
		columns[i%cols] = append(columns[i%cols], it)
	}

	columns = balanceColumns(columns, 4)

	ipResp, _ := http.Get("https://icanhazip.com")
	ipBytes, _ := io.ReadAll(ipResp.Body)
	publicIP := strings.TrimSpace(string(ipBytes))

	pingResult, _ := pingHost(cfg.PingHost)

	temps, symbols := fetchWeather(cfg.Lat, cfg.Lon)
	memUsage := getMemUsage()
	diskUsage := getDiskUsage()

	sunrise, sunset := sunrise.SunriseSunset(cfg.Lat, cfg.Lon, now.Year(), now.Month(), now.Day())

	elPriceCurrent := ""
	elPriceMinMax := ""

	if p, err := fetchCurrentElectricityPrice(cfg.ElectricityArea); err == nil {
		elPriceCurrent = fmt.Sprintf("%.4f SEK/kWh", p[0])
		elPriceMinMax = fmt.Sprintf("Min: %.4f, Max: %.4f", p[1], p[2])
	}

	xr1 := ""
	if p, err := getExchangeRate(cfg.CurrencyFrom1, cfg.CurrencyTo1); err == nil {
		xr1 = fmt.Sprintf("%.2f %s/%s", p, cfg.CurrencyTo1, cfg.CurrencyFrom1)
	}

	xr2 := ""
	if p, err := getExchangeRate(cfg.CurrencyFrom2, cfg.CurrencyTo2); err == nil {
		xr2 = fmt.Sprintf("%.2f %s/%s", p, cfg.CurrencyTo2, cfg.CurrencyFrom2)
	}

	pd := PageData{
		GeneratedAt:             now,
		DateString:              now.Format("Monday, January 2, 2006"),
		Title:                   cfg.Title,
		Fortune:                 getFortune(10, 100),
		Items:                   columns,
		Buttons:                 cfg.Buttons,
		PublicIP:                publicIP,
		PingResult:              pingResult,
		Uptime:                  getUptimeStartDate(),
		SunRise:                 sunrise.Local().Format("15:04"),
		SunSet:                  sunset.Local().Format("15:04"),
		MemUsage:                memUsage,
		MemArc:                  arcPath(memUsage),
		DiskUsage:               diskUsage,
		DiskArc:                 arcPath(diskUsage),
		ElectricityPriceCurrent: elPriceCurrent,
		ElectricityPriceMinMax:  elPriceMinMax,
		ExchangeRate1:           xr1,
		ExchangeRate2:           xr2,
		MemGaugeColor:           cfg.MemGaugeColor,
		DiskGaugeColor:          cfg.DiskGaugeColor,
	}
	if len(temps) >= 2 {
		pd.TodayTemp = fmt.Sprintf("%.1f °C", temps[0])
		pd.TodaySymbol = symbols[0]
		pd.TomorrowTemp = fmt.Sprintf("%.1f °C", temps[1])
		pd.TomorrowSymbol = symbols[1]
	}
	return pd
}

func truncateString(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}

	cut := n
	if cut < len(s) {
		for cut > 0 && s[cut] != ' ' {
			cut--
		}
		if cut == 0 {
			cut = n
		}
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

func stripTags(s string) string {
	out := ""
	inTag := false
	for _, r := range s {
		switch r {
		case '<':
			inTag = true
		case '>':
			inTag = false
		default:
			if !inTag {
				out += string(r)
			}
		}
	}
	return strings.TrimSpace(out)
}

func createDir(dir string) error {
	if stat, err := os.Stat(dir); err != nil || !stat.IsDir() {
		return os.MkdirAll(dir, 0755)
	} else {
		return err
	}
}

func extractImgSrc(htmlText string) string {
	r := strings.NewReader(htmlText)
	z := html.NewTokenizer(r)
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return ""
		case html.StartTagToken, html.SelfClosingTagToken:
			t := z.Token()
			if strings.EqualFold(t.Data, "img") {
				for _, a := range t.Attr {
					if strings.EqualFold(a.Key, "src") && a.Val != "" {
						return a.Val
					}
				}
			}
		}
	}
}
