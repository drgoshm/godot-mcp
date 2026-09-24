package tools

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	navItem = `class_name ItemData
extends Resource

## How strong the item is.
@export var power: int = 1

signal used(by: Node)

## Doubles the power.
func boost(factor: int = 2) -> int:
	return power * factor
`
	navPlayer = `extends CharacterBody2D

const SPEED := 200.0
var inventory: Array[ItemData] = []

func _physics_process(_delta: float) -> void:
	velocity.x = SPEED
	move_and_slide()

func use_first() -> int:
	var item: ItemData = inventory[0]
	item.used.emit(self)
	return item.boost(3) + item.power
`
	navShop = "extends Node\n\nfunc shop(i: ItemData) -> int:\n\treturn i.power + i.boost()\n"
)

func TestCodeNavigation(t *testing.T) {
	call, dir, _ := newSession(t, "Nav")
	writeFiles(t, dir, map[string]string{
		"item.gd": navItem, "player.gd": navPlayer, "shop.gd": navShop,
		"addons/helper/helper.gd": "extends Node\n\nfunc boost_helper() -> void:\n\tpass\n",
	})
	if out, ok := call("godot_import", nil); !ok {
		t.Fatal(out)
	}
	mustOK := func(name string, args map[string]any) (map[string]any, string) {
		t.Helper()
		out, ok := call(name, args)
		if !ok {
			t.Fatalf("%s %v: %v", name, args, out)
		}
		return out, mustJSON(out)
	}
	contains := func(label, text string, wants ...string) {
		t.Helper()
		for _, w := range wants {
			if !strings.Contains(text, w) {
				t.Errorf("%s has no %s:\n%s", label, w, text)
			}
		}
	}

	// Метод другого класса: подсказка с doc-комментарием и переход к объявлению.
	out, j := mustOK("godot_symbol_info", map[string]any{"path": "res://player.gd", "symbol": "boost"})
	contains("boost", j, `"at":"res://player.gd:13:14"`,
		`"definitions":[{"line":10,"path":"res://item.gd","text":"func boost(factor: int = 2) -\u003e int:"}]`)
	contains("boost hover", out["hover"].(string), "func boost(factor: int = 2) -> int", "Doubles the power.", "Defined in res://item.gd")
	if strings.Contains(out["hover"].(string), "file://") {
		t.Errorf("hover should not leak absolute paths: %s", out["hover"])
	}

	// Все использования поля по проекту, включая никогда не открытый shop.gd; объявление не повторяется.
	_, j = mustOK("godot_symbol_info", map[string]any{"path": "res://item.gd", "symbol": "power", "line": 5, "references": true})
	contains("power refs", j, `{"line":11,"path":"res://item.gd","text":"return power * factor"}`,
		`{"line":13,"path":"res://player.gd","text":"return item.boost(3) + item.power"}`,
		`{"line":4,"path":"res://shop.gd","text":"return i.power + i.boost()"}`)
	if strings.Count(j, `"line":5`) != 1 {
		t.Errorf("the declaration must be listed once, as the definition: %s", j)
	}
	refs, _ := call("godot_symbol_info", map[string]any{"path": "res://item.gd", "symbol": "power", "references": true})
	for _, r := range asSlice(refs["references"]) {
		if text := r.(map[string]any)["text"].(string); strings.HasPrefix(text, "#") {
			t.Errorf("a word in a comment is not a usage: %s", text)
		}
	}

	// Член движка: описания на английском, определения в проекте нет.
	out, j = mustOK("godot_symbol_info", map[string]any{"path": "res://player.gd", "symbol": "move_and_slide"})
	contains("engine member", out["hover"].(string), "CharacterBody2D.move_and_slide() -> bool", "Moves the body based on")
	contains("engine member", j, "godot_class_docs")
	if out["definitions"] != nil {
		t.Errorf("engine member has no project definition: %s", j)
	}

	// Файл изменился на диске — определение ищется в новой версии.
	writeFiles(t, dir, map[string]string{"item.gd": strings.Replace(navItem, "## Doubles", "\n\n## Doubles", 1)})
	time.Sleep(20 * time.Millisecond)
	_, j = mustOK("godot_symbol_info", map[string]any{"path": "res://shop.gd", "symbol": "boost"})
	contains("after edit", j, `{"line":12,"path":"res://item.gd","text":"func boost(factor: int = 2) -\u003e int:"}`)

	// Поиск по проекту и структура файла.
	_, j = mustOK("godot_find_symbol", map[string]any{"query": "boost"})
	contains("find boost", j, `{"container":"ItemData","detail":"func boost(factor: int = 2) -\u003e int","doc":"Doubles the power.","kind":"method","line":12,"name":"boost","path":"res://item.gd"}`)
	if strings.Contains(j, "boost_helper") {
		t.Errorf("addons are excluded by default: %s", j)
	}
	_, j = mustOK("godot_find_symbol", map[string]any{"query": "boost", "include_addons": true})
	contains("with addons", j, `"name":"boost_helper"`)
	_, j = mustOK("godot_find_symbol", map[string]any{"path": "res://item.gd"})
	contains("outline", j, `"kind":"class","line":1,"name":"ItemData"`, `"kind":"variable","line":5,"name":"power"`,
		`"kind":"signal","line":7,"name":"used"`, `"doc":"How strong the item is."`)

	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"path": "res://player.gd", "symbol": "boost", "line": 3}, `"boost" is not on line 3`},
		{map[string]any{"path": "res://player.gd", "symbol": "nothing_here"}, "does not occur in this file"},
		{map[string]any{"path": "res://player.gd"}, "pass symbol"},
	} {
		if out, ok := call("godot_symbol_info", tc.args); ok || !strings.Contains(fmt.Sprint(out["error"]), tc.want) {
			t.Errorf("%v: want error %q, got %v", tc.args, tc.want, out)
		}
	}

	callOff, _, _ := newSessionWith(t, "NoLSP", false)
	if out, ok := callOff("godot_find_symbol", map[string]any{"query": "x"}); ok || !strings.Contains(fmt.Sprint(out["error"]), "--lsp=off") {
		t.Errorf("without lsp: %v", out)
	}
}

func TestLocateAndComments(t *testing.T) {
	src := "extends Node\n# boost here is a comment\nvar s := \"boost\"\nfunc f() -> void:\n\tvar x := boost_all + boost(1)\n"
	for _, tc := range []struct {
		symbol       string
		line, col    int
		wantL, wantC int
		wantErr      string
	}{
		{symbol: "boost", wantL: 2, wantC: 10},          // первое вхождение вне комментария (в строке тоже считается)
		{symbol: "boost", line: 5, wantL: 4, wantC: 22}, // целое слово, не boost_all
		{line: 5, col: 3, wantL: 4, wantC: 2},
		{symbol: "missing", wantErr: "does not occur"},
		{symbol: "boost", line: 9, wantErr: "past the end"},
	} {
		l, c, err := locate(src, tc.symbol, tc.line, tc.col)
		switch {
		case tc.wantErr != "":
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%+v: err = %v", tc, err)
			}
		case err != nil || l != tc.wantL || c != tc.wantC:
			t.Errorf("%+v: got %d:%d %v", tc, l, c, err)
		}
	}
	for line, want := range map[string]bool{
		"## Doubles the power.":                true,
		"return power # power in comment":      false, // первое вхождение (col 7) — код
		"var s := \"# not a comment\" + power": false,
	} {
		col := strings.Index(line, "power")
		if got := inComment(line, col); got != want {
			t.Errorf("inComment(%q, %d) = %v", line, col, got)
		}
	}
	// Позиция в символах, а не байтах: кириллица в строке перед именем.
	if _, c, _ := locate("var s := \"привет\"; var boost := 1\n", "boost", 1, 0); c != 23 {
		t.Errorf("rune column = %d, want 23", c)
	}
}
