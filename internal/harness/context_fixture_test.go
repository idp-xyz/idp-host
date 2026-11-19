package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"idp-host/internal/agui"
	"idp-host/tests/fixtures/fakemodel"
)

// recordedLikeThread is a conversation shaped like the long runs of the baseline (D52): a user request in Chinese, then
// pairs of an assistant message with one tool call (a script of a few KB, or a short command) and its result (a table of
// numbers, a listing, an error now and then), a skill resource read near the start, and a closing user message. It is
// generated, not recorded: the recorded conversations are third-party business content and stay out of the repository.
func recordedLikeThread(pairs int) []agui.Message {
	msgs := []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "这个价格表特别慢，请用新的表格完整重新整理，给我下载"}}
	for i := 1; i <= pairs; i++ {
		id := fmt.Sprintf("call_%03d", i)
		var args, result, tool string
		switch {
		case i == 2:
			tool, args, result = "resource__read", `{"skill":"officecli","name":"reference.md"}`, "REFERENCE "+strings.Repeat("how to use officecli; ", 300)
		case i%7 == 0:
			tool, args, result = "sandbox_run", `{"language":"shell","code":"ls -la out/"}`, "exit code 0\n-rw-r--r-- 1 u u 51234 out/价格表.xlsx\n"
		case i%5 == 0:
			tool, args, result = "sandbox_run", `{"language":"python","code":"print(1/0)"}`, "exit code 1\nTraceback (most recent call last):\n  File \"<stdin>\", line 1\nZeroDivisionError: division by zero"
		default:
			script := "import openpyxl\nwb = openpyxl.load_workbook('in/price.xlsx')\n" + strings.Repeat(fmt.Sprintf("for row in ws.iter_rows(min_row=%d):\n    total += row[3].value or 0  # 单价\n", i), 40)
			tool = "sandbox_run"
			b, _ := json.Marshal(map[string]any{"language": "python", "code": script})
			args = string(b)
			var sb strings.Builder
			for r := 0; r < 90; r++ {
				fmt.Fprintf(&sb, "%d | 美国 %d区 | %d.%02d | %d.%03d | 2026-11-%02d\n", i*1000+r, r%9, 10+r, r%100, 3+r%7, (r*37)%1000, 1+r%28)
			}
			result = sb.String()
		}
		msgs = append(msgs,
			agui.Message{ID: "a" + id, Role: agui.RoleAssistant, Content: fmt.Sprintf("第 %d 步", i), ToolCalls: []agui.ToolCall{{ID: id, Type: "function", Function: agui.FunctionCall{Name: tool, Arguments: args}}}},
			agui.Message{ID: "tool_" + id, Role: agui.RoleTool, ToolCallID: id, Content: result})
		if strings.HasPrefix(result, "exit code 1") {
			msgs[len(msgs)-1].Error = "the tool reported an error"
		}
	}
	return append(msgs, agui.Message{ID: "u2", Role: agui.RoleUser, Content: "go on"})
}

// requestDigest is the hash of what the model was sent, without the budget note (it holds the time the run ends).
func requestDigest(sent []fakemodel.Message) string {
	var kept []fakemodel.Message
	for _, m := range sent {
		if !strings.HasPrefix(m.Content, "Budget:") {
			kept = append(kept, m)
		}
	}
	b, _ := json.Marshal(kept)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
