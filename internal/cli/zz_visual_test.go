package cli

import (
	"fmt"
	"testing"
)

func TestVisualDump(t *testing.T) {
	rows := []monitorRow{
		{ThreadID: 62, From: "dir:ledger", Status: "exited", Task: "Investigate a UI bug in the pockets display. Do NOT fix yet."},
		{ThreadID: 40, From: "haydn/his-mouse-friday", Status: "exited", TodosDone: 2, TodosTotal: 2, Task: "Run `hmf sync` in this repo root, then report the count"},
		{ThreadID: 36, From: "haydn/his-mouse-friday", Status: "exited", TodosDone: 3, TodosTotal: 3, Task: "Run `hmf sync` in this repo root, then report the count"},
		{ThreadID: 9, From: "dir:s2s-vpn", Status: "exited", TodosDone: 1, TodosTotal: 1, Task: "Read-only search task (no edits). In the repo"},
		{ThreadID: 1, From: "dir:ledger", Status: "active", TodosDone: 4, TodosTotal: 9, Task: "Bug report (frontend): a stray differ"},
	}
	m := monitorModel{w: 100, rows: rows, cursor: 4}
	fmt.Println("|" + fmt.Sprint(len("|")) + "---- w=46 ----")
	for i := range rows {
		fmt.Println(m.listEntry(i, 46))
	}
}
