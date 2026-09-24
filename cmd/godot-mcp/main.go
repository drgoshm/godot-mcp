// godot-mcp — MCP-сервер, который даёт агенту работать с файлами
// Godot 4.x-проекта и запускать его.
//
//	godot-mcp --project ~/games/my-game [--godot /path/to/Godot]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/drgoshm/godot-mcp/internal/godot"
	"github.com/drgoshm/godot-mcp/internal/tools"
)

var version = "0.1.0"

func main() {
	projectDir := flag.String("project", os.Getenv("GODOT_PROJECT"),
		"initially active Godot project (directory with project.godot); optional, the agent can select one with godot_select_project")
	var roots rootsFlag
	if env := os.Getenv("GODOT_MCP_ROOTS"); env != "" {
		roots = filepath.SplitList(env)
	}
	flag.Var(&roots, "root", "allow projects only under this directory and search it in godot_list_projects (repeatable; $GODOT_MCP_ROOTS)")
	godotBin := flag.String("godot", "", "path to the Godot 4 binary (default: $GODOT_BIN, PATH, standard locations)")
	lspMode := flag.String("lsp", envOr("GODOT_MCP_LSP", "on"),
		"on: check scripts through a background headless editor's language server; off: only godot --check-only")
	flag.Parse()

	// stdout занят протоколом MCP — все логи только в stderr.
	log.SetOutput(os.Stderr)
	log.SetPrefix("godot-mcp: ")
	log.SetFlags(0)

	if *lspMode != "on" && *lspMode != "off" {
		log.Fatalf("--lsp must be on or off, got %q", *lspMode)
	}
	if err := run(*projectDir, *godotBin, roots, *lspMode == "on"); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// rootsFlag — повторяемый флаг --root.
type rootsFlag []string

func (r *rootsFlag) String() string { return strings.Join(*r, string(filepath.ListSeparator)) }

func (r *rootsFlag) Set(v string) error {
	abs, err := filepath.Abs(v)
	if err != nil {
		return err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", v)
	}
	*r = append(*r, abs)
	return nil
}

func run(projectDir, godotBin string, roots []string, useLSP bool) error {
	bin, err := godot.FindBinary(godotBin)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ver, err := (&godot.Godot{Bin: bin}).Version(ctx)
	if err != nil {
		return fmt.Errorf("cannot run %s --version: %w", bin, err)
	}
	if !strings.HasPrefix(ver, "4.") {
		return fmt.Errorf("Godot 4.x is required, got %s", ver)
	}

	// Проекты открываются по требованию; у каждого свои запуски и фоновый редактор.
	ws := &tools.Workspace{Bin: bin, Version: ver, UseLSP: useLSP, Roots: roots}
	defer func() {
		// Не оставляем запущенные игры и редакторы после отключения клиента.
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ws.Close(cleanup)
	}()
	if projectDir != "" {
		if _, err := ws.Select(projectDir); err != nil {
			return err
		}
		log.Printf("project %s, godot %s (%s)", ws.Active(), ver, bin)
	} else {
		log.Printf("no project selected yet, godot %s (%s)", ver, bin)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "godot-mcp", Version: version}, &mcp.ServerOptions{
		Instructions: "Tools for Godot 4 projects. Pick a project first with godot_select_project (godot_list_projects shows known ones) " +
			"unless one is already active; every tool also accepts project to work on another project without switching. " +
			"Paths inside a project are res:// paths. Typical loop: godot_project_info -> " +
			"read/edit files -> godot_check_script -> godot_run_project (quit_after for a smoke test) -> godot_get_output; " +
			"use godot_screenshot to see what the game looks like and godot_run_tests for GUT/gdUnit4 tests. " +
			"Check engine APIs with godot_class_docs instead of relying on memory: many names changed since Godot 3. " +
			"Navigate project code with godot_find_symbol and godot_symbol_info (definition, usages) before changing a function. " +
			"Create scenes with godot_create_scene and change them with godot_scene_tree + godot_edit_scene instead of hand-editing .tscn.",
	})
	tools.Register(server, ws)

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
