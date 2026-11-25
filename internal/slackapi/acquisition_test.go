package slackapi

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAcquisitionRawMessage(t *testing.T) {
	raw := json.RawMessage(`{"ts":"1795046401.000001","user":"U1","text":"hello\u0000 <@U2>","unknown":9007199254740993,"files":[{"id":"F1","name":"a.txt","unknown":9007199254740993}]}`)
	changes, err := AcquisitionChanges(raw, "T1", "C1", SourceUser, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 || *changes[0].Row[16].Value != string(raw) || *changes[1].Row[21].Value != `{"id":"F1","name":"a.txt","unknown":9007199254740993}` {
		t.Fatal("raw fields lost")
	}
	if *changes[0].Row[8].Value != "hello\x00 <@U2>" || *changes[0].Row[14].Value != "1" {
		t.Fatal("native normalization differs")
	}
	for _, s := range []string{"1.+00001", "1.-00000", "1.0000010"} {
		if _, err := AcquisitionTimestamp(s); err == nil {
			t.Fatal("accepted timestamp", s)
		}
	}
}
