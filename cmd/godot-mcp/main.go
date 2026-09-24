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
	"github.com/drgoshm/godot-mcp/internal/project"
	"github.com/drgoshm/godot-mcp/internal/tools"
)

var version = "0.1.0"

func main() {
	projectDir := flag.String("project", os.Getenv("GODOT_PROJECT"), "path to the Godot project (directory with project.godot)")
	godotBin := flag.String("godot", "", "path to the Godot 4 binary (default: $GODOT_BIN, PATH, standard locations)")
	flag.Parse()

	// stdout занят протоколом MCP — все логи только в stderr.
	log.SetOutput(os.Stderr)
	log.SetPrefix("godot-mcp: ")
	log.SetFlags(0)

	if err := run(*projectDir, *godotBin); err != nil {
		log.Fatal(err)
	}
}

func run(projectDir, godotBin string) error {
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
			"use godot_screenshot to see what the game looks like. " +
			"Create scenes with godot_create_scene and change them with godot_scene_tree + godot_edit_scene instead of hand-editing .tscn.",
	})
	tools.Register(server, &tools.Deps{Sandbox: sb, Godot: g, Runner: runner, Version: ver})

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
