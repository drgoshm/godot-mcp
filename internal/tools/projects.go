package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/drgoshm/godot-mcp/internal/docs"
	"github.com/drgoshm/godot-mcp/internal/godot"
	"github.com/drgoshm/godot-mcp/internal/lsp"
	"github.com/drgoshm/godot-mcp/internal/project"
)

// ProjectArg встраивается во входные данные инструментов: необязательный путь
// к проекту. Без него инструмент работает с активным проектом.
type ProjectArg struct {
	Project string `json:"project,omitempty" jsonschema:"path to a Godot project directory (with project.godot); default is the active project, see godot_select_project"`
}

// ProjectOnlyIn — вход инструментов, которым кроме проекта ничего не нужно.
type ProjectOnlyIn struct {
	ProjectArg
}

// Workspace держит открытые проекты: для каждого — своя песочница, запуски,
// мост к игре и фоновый редактор. Один из проектов активный.
type Workspace struct {
	Bin     string       // бинарник Godot
	Version string       // версия движка
	Docs    *docs.Loader // справка по API движка — общая для всех проектов
	UseLSP  bool         // запускать фоновый редактор для проверки и навигации
	// Roots ограничивает, откуда можно открывать проекты; пусто — откуда угодно.
	Roots []string
	// EditorProjects — путь к списку проектов редактора Godot (projects.cfg);
	// "" — стандартное место для ОС, "-" — не читать.
	EditorProjects string

	mu       sync.Mutex
	projects map[string]*Deps // ключ — корень проекта
	active   string
}

var errNoProject = errors.New("no project selected: call godot_select_project with a project path (godot_list_projects shows known projects), or pass project to this tool")

// Deps возвращает проект по пути; пустой путь — активный проект.
func (w *Workspace) Deps(path string) (*Deps, error) {
	if path == "" {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.active == "" {
			return nil, errNoProject
		}
		return w.projects[w.active], nil
	}
	return w.Open(path)
}

// DepsForRun — проект для инструментов запусков: явный project, иначе проект,
// которому принадлежит run_id, иначе активный.
func (w *Workspace) DepsForRun(path, runID string) (*Deps, error) {
	if path == "" && runID != "" {
		w.mu.Lock()
		for _, d := range w.projects {
			if _, err := d.Runner.Get(runID); err == nil {
				w.mu.Unlock()
				return d, nil
			}
		}
		w.mu.Unlock()
	}
	return w.Deps(path)
}

// Open открывает проект (или возвращает уже открытый), не меняя активный.
func (w *Workspace) Open(path string) (*Deps, error) {
	root, err := w.normalize(path)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if d, ok := w.projects[root]; ok {
		return d, nil
	}
	sb, err := project.NewSandbox(root)
	if err != nil {
		return nil, err
	}
	g := &godot.Godot{Bin: w.Bin, ProjectDir: sb.Root()}
	d := &Deps{Sandbox: sb, Godot: g, Runner: godot.NewRunner(g), Version: w.Version, Docs: w.Docs}
	if w.UseLSP {
		d.LSP = &lsp.Checker{Bin: w.Bin, ProjectDir: sb.Root()}
	}
	if w.projects == nil {
		w.projects = map[string]*Deps{}
	}
	w.projects[root] = d
	return d, nil
}

// Select делает проект активным.
func (w *Workspace) Select(path string) (*Deps, error) {
	d, err := w.Open(path)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.active = d.Sandbox.Root()
	w.mu.Unlock()
	return d, nil
}

// Active — корень активного проекта или "".
func (w *Workspace) Active() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.active
}

// Close останавливает игры и фоновые редакторы всех проектов.
func (w *Workspace) Close(ctx context.Context) {
	w.mu.Lock()
	all := make([]*Deps, 0, len(w.projects))
	for _, d := range w.projects {
		all = append(all, d)
	}
	w.mu.Unlock()
	for _, d := range all {
		d.Runner.StopAll(ctx)
		if d.LSP != nil {
			d.LSP.Close()
		}
	}
}

// normalize: ~, относительный путь, путь к project.godot -> абсолютный корень
// проекта; проверка на --root.
func (w *Workspace) normalize(path string) (string, error) {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	if filepath.Base(path) == "project.godot" {
		path = filepath.Dir(path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if _, err := os.Stat(filepath.Join(abs, "project.godot")); err != nil {
		return "", fmt.Errorf("%s is not a Godot project (no project.godot); godot_list_projects shows known projects", abs)
	}
	if !w.allowed(abs) {
		return "", fmt.Errorf("%s is outside the allowed project roots (%s)", abs, strings.Join(w.Roots, ", "))
	}
	return abs, nil
}

func (w *Workspace) allowed(abs string) bool {
	if len(w.Roots) == 0 {
		return true
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return false
	}
	for _, r := range w.Roots {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		if real == rr || strings.HasPrefix(real, rr+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// ---- инструменты ----

type SelectProjectIn struct {
	Path string `json:"path" jsonschema:"Godot project directory (with project.godot) or the project.godot file itself; ~ is expanded"`
}

type ListProjectsIn struct {
	Query string `json:"query,omitempty" jsonschema:"only projects whose name or path contains this text"`
}

type KnownProject struct {
	Path     string `json:"path"`
	Name     string `json:"name,omitempty"`
	Active   bool   `json:"active,omitempty"`
	Open     bool   `json:"open,omitempty"`     // уже открыт в этой сессии
	Favorite bool   `json:"favorite,omitempty"` // отмечен в менеджере проектов Godot
	Source   string `json:"source"`             // session | godot_editor | root
}

type ListProjectsOut struct {
	Projects []KnownProject `json:"projects"`
	Active   string         `json:"active,omitempty"`
	Roots    []string       `json:"roots,omitempty"`
}

func registerProjectTools(s *mcp.Server, w *Workspace) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "godot_select_project",
		Description: "Make a Godot project active: tools without a project argument work on it. " +
			"Returns the same summary as godot_project_info. Several projects can be open at once; " +
			"pass project to any tool to use another one without switching.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SelectProjectIn) (*mcp.CallToolResult, ProjectInfoOut, error) {
		if in.Path == "" {
			return nil, ProjectInfoOut{}, errors.New("path is required")
		}
		d, err := w.Select(in.Path)
		if err != nil {
			return nil, ProjectInfoOut{}, err
		}
		return projectInfo(d)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "godot_list_projects",
		Description: "List Godot projects you can select: those opened in this session, those in the Godot editor's " +
			"project manager (favorites first), and projects found under the server's allowed roots.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ListProjectsIn) (*mcp.CallToolResult, ListProjectsOut, error) {
		return nil, w.list(in.Query), nil
	})
}

func (w *Workspace) list(query string) ListProjectsOut {
	out := ListProjectsOut{Active: w.Active(), Roots: w.Roots, Projects: []KnownProject{}}
	seen := map[string]int{}
	add := func(p KnownProject) {
		if i, ok := seen[p.Path]; ok {
			out.Projects[i].Favorite = out.Projects[i].Favorite || p.Favorite
			return
		}
		if !w.allowed(p.Path) {
			return
		}
		if _, err := os.Stat(filepath.Join(p.Path, "project.godot")); err != nil {
			return
		}
		p.Name = projectName(p.Path)
		p.Active = p.Path == out.Active
		seen[p.Path] = len(out.Projects)
		out.Projects = append(out.Projects, p)
	}

	w.mu.Lock()
	var open []string
	for root := range w.projects {
		open = append(open, root)
	}
	w.mu.Unlock()
	sort.Strings(open)
	for _, root := range open {
		add(KnownProject{Path: root, Open: true, Source: "session"})
	}
	for _, p := range editorProjects(w.EditorProjects) {
		add(p)
	}
	for _, r := range w.Roots {
		for _, p := range scanRoot(r, 4) {
			add(KnownProject{Path: p, Source: "root"})
		}
	}

	if q := strings.ToLower(query); q != "" {
		filtered := out.Projects[:0]
		for _, p := range out.Projects {
			if strings.Contains(strings.ToLower(p.Name), q) || strings.Contains(strings.ToLower(p.Path), q) {
				filtered = append(filtered, p)
			}
		}
		out.Projects = filtered
	}
	// Активный и открытые — сверху, потом избранные.
	sort.SliceStable(out.Projects, func(i, j int) bool {
		rank := func(p KnownProject) int {
			switch {
			case p.Active:
				return 0
			case p.Open:
				return 1
			case p.Favorite:
				return 2
			}
			return 3
		}
		return rank(out.Projects[i]) < rank(out.Projects[j])
	})
	return out
}

// editorProjects читает список менеджера проектов Godot: секции [путь] с favorite=.
func editorProjects(cfg string) []KnownProject {
	if cfg == "-" {
		return nil
	}
	if cfg == "" {
		cfg = defaultEditorProjects()
	}
	f, err := os.Open(cfg)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []KnownProject
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			out = append(out, KnownProject{Path: filepath.Clean(line[1 : len(line)-1]), Source: "godot_editor"})
		case strings.HasPrefix(line, "favorite=") && len(out) > 0:
			out[len(out)-1].Favorite = strings.TrimPrefix(line, "favorite=") == "true"
		}
	}
	return out
}

func defaultEditorProjects() string {
	switch runtime.GOOS {
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "Godot", "projects.cfg")
		}
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), "Godot", "projects.cfg")
	default:
		dir := os.Getenv("XDG_DATA_HOME")
		if dir == "" {
			if home, err := os.UserHomeDir(); err == nil {
				dir = filepath.Join(home, ".local", "share")
			}
		}
		return filepath.Join(dir, "godot", "projects.cfg")
	}
	return ""
}

// scanRoot ищет каталоги с project.godot не глубже depth уровней.
func scanRoot(root string, depth int) []string {
	root = filepath.Clean(root)
	base := strings.Count(root, string(filepath.Separator))
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if n := d.Name(); p != root && (strings.HasPrefix(n, ".") || n == "node_modules" || n == "addons") {
			return fs.SkipDir
		}
		if _, err := os.Stat(filepath.Join(p, "project.godot")); err == nil {
			out = append(out, p)
			return fs.SkipDir // вложенных проектов не бывает
		}
		if strings.Count(p, string(filepath.Separator))-base >= depth {
			return fs.SkipDir
		}
		return nil
	})
	return out
}

// projectName — config/name из project.godot.
func projectName(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "project.godot"))
	if err != nil {
		return ""
	}
	cfg := project.ParseConfig(string(data))
	return project.Unquote(cfg["application"]["config/name"])
}
