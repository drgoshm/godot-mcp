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
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/drgoshm/godot-mcp/internal/godot"
	"github.com/drgoshm/godot-mcp/internal/lsp"
	"github.com/drgoshm/godot-mcp/internal/project"
	"github.com/drgoshm/godot-mcp/internal/tools"
)

var version = "0.1.0"

func main() {
	projectDir := flag.String("project", os.Getenv("GODOT_PROJECT"), "path to the Godot project (directory with project.godot)")
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
	if err := run(*projectDir, *godotBin, *lspMode == "on"); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run(projectDir, godotBin string, useLSP bool) error {
	if projectDir == "" {
		return fmt.Errorf("--project is required (or set GODOT_PROJECT)")
	}
	sb, err := project.NewSandbox(projectDir)
	if err != nil {
		return err
	}
	bin, err := godot.FindBinary(godotBin)
	if err != nil {
		return err
	}
	g := &godot.Godot{Bin: bin, ProjectDir: sb.Root()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ver, err := g.Version(ctx)
	if err != nil {
		return fmt.Errorf("cannot run %s --version: %w", bin, err)
	}
	if !strings.HasPrefix(ver, "4.") {
		return fmt.Errorf("Godot 4.x is required, got %s", ver)
	}
	log.Printf("project %s, godot %s (%s)", sb.Root(), ver, bin)

	runner := godot.NewRunner(g)
	// Не оставляем запущенные игры после отключения клиента.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		runner.StopAll(cleanup)
	}()

	server := mcp.NewServer(&mcp.Implementation{Name: "godot-mcp", Version: version}, &mcp.ServerOptions{
		Instructions: "Tools for a Godot 4 project. Paths are res:// paths. Typical loop: godot_project_info -> " +
			"read/edit files -> godot_check_script -> godot_run_project (quit_after for a smoke test) -> godot_get_output; " +
			"use godot_screenshot to see what the game looks like and godot_run_tests for GUT/gdUnit4 tests. " +
			"Check engine APIs with godot_class_docs instead of relying on memory: many names changed since Godot 3. " +
			"Navigate project code with godot_find_symbol and godot_symbol_info (definition, usages) before changing a function. " +
			"Create scenes with godot_create_scene and change them with godot_scene_tree + godot_edit_scene instead of hand-editing .tscn.",
	})
	deps := &tools.Deps{Sandbox: sb, Godot: g, Runner: runner, Version: ver}
	if useLSP {
		// Редактор стартует при первой проверке скрипта и сам останавливается в простое.
		deps.LSP = &lsp.Checker{Bin: bin, ProjectDir: sb.Root()}
		defer deps.LSP.Close()
	}
	tools.Register(server, deps)

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
