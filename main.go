package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

// ---- Settings: adjust to match how your nightly builds are named ----
// Nightly release tags look like v0.4.2-nightly.20261023 (the date is the build day).
var nightlyRelease = regexp.MustCompile(`-nightly\.(\d{8})$`)

// The release workflow (named e.g. "Release MyApp") runs on a tag push; for such runs
// headBranch is the tag name. Any workflow whose name starts with this prefix is used.
const releaseWorkflowPrefix = "Release"

// Given workflow name determine if it is a "Release" workflow.
func isReleaseWorkflow(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), strings.ToLower(releaseWorkflowPrefix))
}

var anyNightly = regexp.MustCompile(`(?i)nightly`)

// GitHub organization names: letters, digits and hyphens, no leading hyphen, max 39 chars.
// Validating keeps a typed or tampered config value from being read by gh as a command-line flag.
var validOrg = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// clean makes text that came from GitHub (tag names, errors) safe to show: control and
// bidi-override characters are removed and the length is capped.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			return ' '
		case unicode.IsControl(r), r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, s)
	if rs := []rune(s); len(rs) > 300 {
		s = string(rs[:300]) + "…"
	}
	return s
}

const (
	workers   = 6                // parallel gh calls
	numDays   = 14               // days fetched; the UI shows the first 3/7/14
	refreshEv = 5 * time.Minute  // auto-refresh interval
	ghTimeout = 60 * time.Second // a hung gh call is killed after this
	dayLayout = "2006-01-02"
)

var (
	colOK   = color.NRGBA{0x3f, 0xb9, 0x50, 0xff}
	colWarn = color.NRGBA{0xe3, 0xb3, 0x41, 0xff}
	colBad  = color.NRGBA{0xf8, 0x51, 0x49, 0xff}
	colNone = color.NRGBA{0x8b, 0x94, 0xa3, 0x70}
)

// ---------- data: gh CLI ----------

type repo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type release struct {
	TagName     string `json:"tagName"`
	Name        string `json:"name"`
	PublishedAt string `json:"publishedAt"`
}

type workflow struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type run struct {
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	CreatedAt    string `json:"createdAt"`
	WorkflowName string `json:"workflowName"`
	Event        string `json:"event"`
	HeadBranch   string `json:"headBranch"` // the tag name for tag-triggered runs
	URL          string `json:"url"`
}

type dayCell struct {
	State string // "ok", "warn", "bad" or ""
	Tip   string
	URL   string
}

type row struct {
	Cells []dayCell
	Err   string
}

type config struct {
	Org  string `json:"org"`
	Days int    `json:"days"`
}

func gh(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	hide(cmd)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, errors.New("gh timed out")
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("gh CLI not found. Install it from https://cli.github.com")
		}
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return out.Bytes(), nil
}

func dayOf(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return ""
	}
	return t.Local().Format(dayLayout)
}

// runDay returns the day a Release run belongs to: the date in a nightly tag
// (v0.4.2-nightly.20261023), or the run's start day for other tags containing "nightly".
// It returns "" for runs that are not nightly runs.
func runDay(r run) string {
	if m := nightlyRelease.FindStringSubmatch(r.HeadBranch); m != nil {
		if t, err := time.Parse("20060102", m[1]); err == nil {
			return t.Format(dayLayout)
		}
	}
	if anyNightly.MatchString(r.HeadBranch) {
		return dayOf(r.CreatedAt)
	}
	return ""
}

func cellFor(rels []release, runs []run, day string) dayCell {
	for _, r := range rels {
		m := nightlyRelease.FindStringSubmatch(r.TagName)
		if m == nil {
			continue
		}
		if t, err := time.Parse("20060102", m[1]); err == nil && t.Format(dayLayout) == day {
			return dayCell{State: "ok", Tip: "Release " + r.TagName}
		}
	}
	var today []run
	for _, r := range runs {
		if isReleaseWorkflow(r.WorkflowName) && runDay(r) == day {
			today = append(today, r)
		}
	}
	for _, r := range today {
		if r.Status != "completed" {
			return dayCell{State: "warn", Tip: r.HeadBranch + ": " + r.Status, URL: r.URL}
		}
	}
	if len(today) > 0 { // runs come newest first, so a successful re-run clears an earlier failure
		r := today[0]
		switch r.Conclusion {
		case "failure", "timed_out", "startup_failure", "cancelled":
			return dayCell{State: "bad", Tip: r.HeadBranch + ": " + r.Conclusion, URL: r.URL}
		}
	}
	return dayCell{Tip: "No nightly found"}
}

func lastDays(n int) []string {
	days := make([]string, n)
	for i := range days {
		days[i] = time.Now().AddDate(0, 0, -i).Format(dayLayout)
	}
	return days
}

func fetchRow(org string, r repo, days []string) row {
	full := org + "/" + r.Name
	var rels []release
	relOut, relErr := gh("release", "list", "-R", full, "--limit", "40", "--json", "tagName,name,publishedAt")
	if relErr == nil {
		json.Unmarshal(relOut, &rels)
	}
	// Find the release workflow(s) by name prefix, then list their runs.
	var runs []run
	var wfs []workflow
	wfOut, runErr := gh("workflow", "list", "-R", full, "--limit", "100", "--json", "id,name")
	if runErr == nil {
		json.Unmarshal(wfOut, &wfs)
		found := false
		for _, wf := range wfs {
			if !isReleaseWorkflow(wf.Name) {
				continue
			}
			found = true
			runOut, err := gh("run", "list", "-R", full, "--workflow", strconv.FormatInt(wf.ID, 10), "--limit", "60", "--json",
				"status,conclusion,createdAt,workflowName,event,headBranch,url")
			if err != nil {
				runErr = err
				continue
			}
			var rs []run
			json.Unmarshal(runOut, &rs)
			runs = append(runs, rs...)
		}
		if !found {
			runErr = errors.New(`no workflow whose name starts with "` + releaseWorkflowPrefix + `"`)
		}
		sort.SliceStable(runs, func(a, b int) bool { return runs[a].CreatedAt > runs[b].CreatedAt }) // newest first
	}
	out := row{}
	if relErr != nil && runErr != nil {
		out.Err = relErr.Error()
	}
	for _, d := range days {
		out.Cells = append(out.Cells, cellFor(rels, runs, d))
	}
	// Grey cells explain themselves: click one to see why nothing was found.
	note := ""
	switch {
	case runErr != nil:
		note = "run list failed: " + runErr.Error()
	default:
		note = "no nightly-tag runs of the " + releaseWorkflowPrefix + " workflow in its last 60 runs"
		for _, r := range runs {
			if anyNightly.MatchString(r.HeadBranch) {
				note = fmt.Sprintf("latest nightly run: %s (%s %s)", r.HeadBranch, r.Status, r.Conclusion)
				break
			}
		}
	}
	for i := range out.Cells {
		if out.Cells[i].State == "" {
			out.Cells[i].Tip += " · " + note
		}
	}
	return out
}

// ---------- model (written by background goroutines, read by the UI) ----------

type model struct {
	mu      sync.Mutex
	gen     int64
	dirty   bool
	org     string
	repos   []repo
	dates   []string
	rows    map[string]row
	loading bool
	errMsg  string
	total   int
	done    int
	updated string
}

func (m *model) update(cur func() bool, fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur() {
		fn()
		m.dirty = true
	}
}

func (m *model) takeDirty() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.dirty
	m.dirty = false
	return d
}

func (m *model) start(org string) {
	if !validOrg.MatchString(org) {
		m.mu.Lock()
		m.errMsg, m.loading, m.dirty = "That is not a valid GitHub organization name.", false, true
		m.mu.Unlock()
		return
	}
	gen := atomic.AddInt64(&m.gen, 1)
	cur := func() bool { return atomic.LoadInt64(&m.gen) == gen }
	m.mu.Lock()
	if m.org != org { // new org: drop old data; same org: keep it until replaced
		m.org, m.rows, m.repos = org, map[string]row{}, nil
	}
	m.loading, m.errMsg, m.done, m.dirty = true, "", 0, true
	m.mu.Unlock()

	go func() {
		out, err := gh("repo", "list", org, "--limit", "300", "--no-archived", "--json", "name,url")
		if err != nil {
			m.update(cur, func() {
				m.errMsg = err.Error() + " (if not signed in, run: gh auth login)"
				m.loading = false
			})
			return
		}
		var repos []repo
		json.Unmarshal(out, &repos)
		sort.Slice(repos, func(i, j int) bool { return strings.ToLower(repos[i].Name) < strings.ToLower(repos[j].Name) })
		days := lastDays(numDays)
		m.update(cur, func() { m.repos, m.dates, m.total = repos, days, len(repos) })

		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup
		for _, r := range repos {
			wg.Add(1)
			sem <- struct{}{}
			go func(r repo) {
				defer wg.Done()
				defer func() { <-sem }()
				if !cur() {
					return
				}
				rw := fetchRow(org, r, days)
				m.update(cur, func() { m.rows[r.Name] = rw; m.done++ })
			}(r)
		}
		wg.Wait()
		m.update(cur, func() { m.loading = false; m.updated = time.Now().Format("15:04:05") })
	}()
}

// ---------- config + links ----------

func cfgPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "gg-nightly", "config.json")
}

func loadConfig() config {
	c := config{Days: 7}
	if b, err := os.ReadFile(cfgPath()); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(c config) {
	b, _ := json.Marshal(c)
	os.MkdirAll(filepath.Dir(cfgPath()), 0o700)
	os.WriteFile(cfgPath(), b, 0o600)
}

func openURL(u string) {
	pu, err := url.Parse(u)
	if err != nil || pu.Scheme != "https" || pu.Host != "github.com" || strings.IndexFunc(u, unicode.IsControl) >= 0 {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	hide(cmd)
	if cmd.Start() == nil {
		go cmd.Wait() // reap the child so it does not linger as a zombie
	}
}

// ---------- UI ----------

// cell is one table cell: a label (repo name column) or a coloured dot (day columns).
type cell struct {
	widget.BaseWidget
	label *widget.Label
	dot   *canvas.Circle
	box   *fyne.Container
}

func newCell() *cell {
	c := &cell{label: widget.NewLabel(""), dot: canvas.NewCircle(color.Transparent)}
	c.label.Truncation = fyne.TextTruncateEllipsis
	c.box = container.NewCenter(container.NewGridWrap(fyne.NewSize(18, 18), c.dot))
	c.ExtendBaseWidget(c)
	return c
}

func (c *cell) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(container.NewStack(c.label, c.box))
}

func (c *cell) showText(t string) {
	c.label.SetText(t)
	c.label.Show()
	c.box.Hide()
}

func (c *cell) showDot(fill, stroke color.Color, width float32) {
	c.label.Hide()
	c.box.Show()
	c.dot.FillColor, c.dot.StrokeColor, c.dot.StrokeWidth = fill, stroke, width
	c.dot.Refresh()
}

type viewRow struct {
	repo repo
	row  row
	ok   bool // false while this repo is still loading
}

func legendItem(col color.Color, text string) fyne.CanvasObject {
	return container.NewHBox(container.NewGridWrap(fyne.NewSize(12, 12), canvas.NewCircle(col)), widget.NewLabel(text))
}

func main() {
	a := app.NewWithID("local.gg-status")
	m := &model{rows: map[string]row{}}
	cfg := loadConfig()
	if cfg.Days != 3 && cfg.Days != 14 {
		cfg.Days = 7
	}
	daysN := cfg.Days

	// view is only touched on the UI thread: a snapshot of the model, filtered.
	var view []viewRow
	var dates []string
	var table *widget.Table
	orgEntry := widget.NewEntry()
	orgEntry.SetPlaceHolder("GitHub organization")
	orgEntry.SetText(cfg.Org)
	filterEntry := widget.NewEntry()
	filterEntry.SetPlaceHolder("Filter repos")
	info := widget.NewLabel("")
	info.Wrapping = fyne.TextWrapWord
	detail := widget.NewLabel("Click a repo to open it. Click a dot for details (yellow/red also opens the run).")
	detail.Wrapping = fyne.TextWrapWord

	rebuild := func() {
		m.mu.Lock()
		q := strings.ToLower(filterEntry.Text)
		view = nil
		for _, r := range m.repos {
			if strings.Contains(strings.ToLower(r.Name), q) {
				rw, ok := m.rows[r.Name]
				view = append(view, viewRow{r, rw, ok})
			}
		}
		dates = append([]string(nil), m.dates...)
		switch {
		case m.errMsg != "":
			info.SetText(clean("Error: " + m.errMsg))
		case m.loading:
			info.SetText(fmt.Sprintf("Loading… %d/%d", m.done, m.total))
		case m.updated != "":
			info.SetText(fmt.Sprintf("%d repos · updated %s", len(m.repos), m.updated))
		case orgEntry.Text == "":
			info.SetText("Enter your GitHub organization and press Enter.")
		}
		m.mu.Unlock()
		table.Refresh()
	}

	table = widget.NewTableWithHeaders(
		func() (int, int) { return len(view), 1 + daysN },
		func() fyne.CanvasObject { return newCell() },
		func(id widget.TableCellID, o fyne.CanvasObject) {
			c := o.(*cell)
			if id.Row >= len(view) {
				c.showText("")
				return
			}
			vr := view[id.Row]
			if id.Col == 0 {
				c.showText(clean(vr.repo.Name))
				return
			}
			i := id.Col - 1
			switch {
			case !vr.ok:
				c.showDot(color.Transparent, colNone, 2) // hollow ring while loading
			case vr.row.Err != "" || i >= len(vr.row.Cells):
				c.showDot(colNone, color.Transparent, 0)
			default:
				col := colNone
				switch vr.row.Cells[i].State {
				case "ok":
					col = colOK
				case "warn":
					col = colWarn
				case "bad":
					col = colBad
				}
				c.showDot(col, color.Transparent, 0)
			}
		})
	table.ShowHeaderColumn = false
	table.CreateHeader = func() fyne.CanvasObject {
		l := widget.NewLabel("")
		l.Alignment = fyne.TextAlignCenter
		return l
	}
	table.UpdateHeader = func(id widget.TableCellID, o fyne.CanvasObject) {
		l, ok := o.(*widget.Label)
		if !ok || id.Row != -1 {
			return
		}
		switch {
		case id.Col == 0:
			l.Alignment = fyne.TextAlignLeading
			l.SetText("Repository")
		case id.Col-1 < len(dates):
			l.Alignment = fyne.TextAlignCenter
			l.SetText(dates[id.Col-1])
		default:
			l.SetText("")
		}
	}
	table.SetColumnWidth(0, 300)
	for i := 1; i <= numDays; i++ {
		table.SetColumnWidth(i, 110)
	}
	table.OnSelected = func(id widget.TableCellID) {
		table.Unselect(id)
		if id.Row < 0 || id.Row >= len(view) {
			return
		}
		vr := view[id.Row]
		if id.Col == 0 {
			openURL(vr.repo.URL)
			return
		}
		i := id.Col - 1
		if !vr.ok || i >= len(vr.row.Cells) || i >= len(dates) {
			return
		}
		c := vr.row.Cells[i]
		detail.SetText(clean(fmt.Sprintf("%s · %s · %s", vr.repo.Name, dates[i], c.Tip)))
		if c.URL != "" {
			openURL(c.URL)
		}
	}

	load := func() {
		org := strings.TrimSpace(orgEntry.Text)
		if org == "" {
			info.SetText("Enter your GitHub organization and press Enter.")
			return
		}
		if !validOrg.MatchString(org) {
			info.SetText("That is not a valid GitHub organization name.")
			return
		}
		saveConfig(config{Org: org, Days: daysN})
		m.start(org)
	}
	orgEntry.OnSubmitted = func(string) { load() }
	filterEntry.OnChanged = func(string) { rebuild() }
	daysSel := widget.NewSelect([]string{"3 days", "7 days", "14 days"}, func(s string) {
		n, _ := strconv.Atoi(strings.Fields(s)[0])
		daysN = n
		saveConfig(config{Org: strings.TrimSpace(orgEntry.Text), Days: daysN})
		rebuild()
	})
	daysSel.Selected = fmt.Sprintf("%d days", daysN)
	refreshBtn := widget.NewButton("Refresh", load)

	top := container.NewVBox(
		container.NewGridWithColumns(4, orgEntry, daysSel, filterEntry, refreshBtn),
		container.NewHBox(
			legendItem(colOK, "Nightly release exists"), legendItem(colWarn, "Build pending"),
			legendItem(colBad, "Build failed"), legendItem(colNone, "No nightly that day"),
		),
	)
	bottom := container.NewVBox(detail, info)

	w := a.NewWindow("Nightly status")
	w.SetContent(container.NewBorder(top, bottom, nil, nil, table))
	w.Resize(fyne.NewSize(1100, 760))

	if cfg.Org != "" {
		m.start(cfg.Org)
	}
	a.Lifecycle().SetOnStarted(func() {
		go func() { // push background changes into the table a few times a second
			for range time.Tick(300 * time.Millisecond) {
				if m.takeDirty() {
					fyne.Do(rebuild)
				}
			}
		}()
		go func() { // auto-refresh
			for range time.Tick(refreshEv) {
				m.mu.Lock()
				org := m.org
				m.mu.Unlock()
				if org != "" {
					m.start(org)
				}
			}
		}()
	})
	w.ShowAndRun()
}
