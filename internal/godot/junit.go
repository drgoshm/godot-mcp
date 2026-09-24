package godot

import (
	"encoding/xml"
	"regexp"
	"strconv"
	"strings"
)

// TestCase — результат одного теста из JUnit-отчёта GUT или gdUnit4.
type TestCase struct {
	Suite   string `json:"suite"`
	Name    string `json:"name"`
	Status  string `json:"status"` // passed | failed | error | skipped
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message,omitempty"`
}

// TestTotals — сводка по отчёту.
type TestTotals struct {
	Tests   int `json:"tests"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Errors  int `json:"errors"`
	Skipped int `json:"skipped"`
}

type junitProblem struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Failure   *junitProblem `xml:"failure"`
	Error     *junitProblem `xml:"error"`
	Skipped   *junitProblem `xml:"skipped"`
}

type junitSuite struct {
	Name  string      `xml:"name,attr"`
	Cases []junitCase `xml:"testcase"`
}

var (
	// gdUnit4: `FAILED: res://test/calc_test.gd:7`, "at 'test_x' in res://test/calc_test.gd:7"
	resLineRe = regexp.MustCompile(`(res://[^\s:'"]+\.gd):(\d+)`)
	// GUT: "at line 7" (-1, если строка неизвестна)
	gutLineRe = regexp.MustCompile(`at line (-?\d+)`)
	// та же строка отдельно — в сообщении не нужна, номер уже в Line
	gutAtLineRe = regexp.MustCompile(`^at line -?\d+$`)
)

// maxMessage — сообщение об ошибке теста длиннее этого обрезается.
const maxMessage = 2000

// ParseJUnit разбирает JUnit XML обоих фреймворков. Корень — <testsuites>
// (или сразу <testsuite>).
func ParseJUnit(data []byte) ([]TestCase, TestTotals, error) {
	var doc struct {
		XMLName xml.Name
		Suites  []junitSuite `xml:"testsuite"`
		Name    string       `xml:"name,attr"`
		Cases   []junitCase  `xml:"testcase"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, TestTotals{}, err
	}
	suites := doc.Suites
	if doc.XMLName.Local == "testsuite" {
		suites = []junitSuite{{Name: doc.Name, Cases: doc.Cases}}
	}

	var cases []TestCase
	var totals TestTotals
	for _, s := range suites {
		for _, c := range s.Cases {
			tc := TestCase{Suite: s.Name, Name: c.Name, Status: "passed", File: suiteFile(s.Name, c.Classname)}
			var p *junitProblem
			switch {
			case c.Error != nil:
				tc.Status, p = "error", c.Error
				totals.Errors++
			case c.Failure != nil:
				tc.Status, p = "failed", c.Failure
				totals.Failed++
			case c.Skipped != nil:
				tc.Status, p = "skipped", c.Skipped
				totals.Skipped++
			default:
				totals.Passed++
			}
			if p != nil {
				tc.Message = cleanMessage(p.Text, p.Message)
				locate(&tc, p.Message+"\n"+p.Text)
			}
			cases = append(cases, tc)
			totals.Tests++
		}
	}
	return cases, totals, nil
}

// suiteFile: gdUnit4 пишет в name/classname имя класса, GUT — путь без res://
// ("test/unit/test_calc.gd", у внутренних классов с суффиксом ".Inner").
func suiteFile(suite, classname string) string {
	for _, s := range []string{suite, classname} {
		if i := strings.Index(s, ".gd"); i > 0 {
			p := s[:i+3]
			if !strings.HasPrefix(p, "res://") {
				p = "res://" + p
			}
			return p
		}
	}
	return ""
}

func locate(tc *TestCase, text string) {
	if m := resLineRe.FindStringSubmatch(text); m != nil {
		tc.File = m[1]
		tc.Line, _ = strconv.Atoi(m[2])
		return
	}
	if m := gutLineRe.FindStringSubmatch(text); m != nil {
		if n, _ := strconv.Atoi(m[1]); n > 0 {
			tc.Line = n
		}
	}
}

// cleanMessage убирает отступы и пустые строки из CDATA; "failed"/"pending" в
// атрибуте GUT ничего не добавляют, поэтому атрибут берём, только если текста нет.
func cleanMessage(text, attr string) string {
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" && !gutAtLineRe.MatchString(l) {
			lines = append(lines, l)
		}
	}
	msg := strings.Join(lines, "\n")
	if msg == "" {
		msg = attr
	}
	if len(msg) > maxMessage {
		msg = msg[:maxMessage] + "…"
	}
	return msg
}
