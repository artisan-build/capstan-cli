//go:build !windows

package work

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStdioSoloClientFramesCallsAndBoundsFailures(t *testing.T) {
	t.Run("line framed handshake and tool call", func(t *testing.T) {
		path := writeMCPPeer(t, `
import json, sys
for line in sys.stdin:
    msg=json.loads(line)
    if 'id' not in msg: continue
    if msg['method']=='initialize': result={}
    else:
        name=msg['params']['name']
        result={'content':[{'type':'text','text':json.dumps({'name':name,'arguments':msg['params']['arguments']})}]}
    print(json.dumps({'jsonrpc':'2.0','id':msg['id'],'result':result}), flush=True)
`)
		client, err := NewStdioSoloClient(context.Background(), path, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = client.Close() }()
		var result struct {
			Name      string         `json:"name"`
			Arguments map[string]int `json:"arguments"`
		}
		if err := client.Call(context.Background(), "echo", map[string]int{"value": 7}, &result); err != nil {
			t.Fatal(err)
		}
		if result.Name != "echo" || result.Arguments["value"] != 7 {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("method errors are redacted", func(t *testing.T) {
		path := writeMCPPeer(t, `
import json, sys
for line in sys.stdin:
    msg=json.loads(line)
    if 'id' not in msg: continue
    print(json.dumps({'jsonrpc':'2.0','id':msg['id'],'error':{'code':-32000,'message':'signed-secret'}}), flush=True)
`)
		_, err := NewStdioSoloClient(context.Background(), path, time.Second)
		if err == nil || strings.Contains(err.Error(), "signed-secret") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("timeout kills hung peer", func(t *testing.T) {
		path := writeMCPPeer(t, `
import sys, time
for line in sys.stdin: time.sleep(10)
`)
		started := time.Now()
		_, err := NewStdioSoloClient(context.Background(), path, 30*time.Millisecond)
		if err == nil || time.Since(started) > time.Second {
			t.Fatalf("error, elapsed = %v, %s", err, time.Since(started))
		}
	})

	t.Run("oversized message is refused", func(t *testing.T) {
		path := writeMCPPeer(t, `
import sys
for line in sys.stdin:
    print('x' * 1100000, flush=True)
`)
		_, err := NewStdioSoloClient(context.Background(), path, time.Second)
		if err == nil {
			t.Fatal("oversized MCP response was accepted")
		}
	})
}

func TestStdioSoloClientBoundsBlockedRequestWriteAndReapsPeer(t *testing.T) {
	path := writeMCPPeer(t, `
import json, sys, time
for line in sys.stdin:
    msg=json.loads(line)
    if 'id' not in msg: continue
    if msg['method']=='initialize':
        result={}
    else:
        result={'content':[{'type':'text','text':'{}'}]}
    print(json.dumps({'jsonrpc':'2.0','id':msg['id'],'result':result}), flush=True)
    if msg['method']=='tools/call' and msg['params']['name']=='identify_session':
        time.sleep(10)
        break
`)
	client, err := NewStdioSoloClient(context.Background(), path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.timeout = 30 * time.Millisecond
	secret := "signed-secret-" + strings.Repeat("x", 4082)
	extraArgs := make([]string, 64)
	for index := range extraArgs {
		extraArgs[index] = secret
	}
	started := time.Now()
	err = client.Call(context.Background(), "spawn_agent", map[string]any{"extra_args": extraArgs}, nil)
	if err == nil || time.Since(started) > time.Second || strings.Contains(err.Error(), "signed-secret") {
		t.Fatalf("error, elapsed = %v, %s", err, time.Since(started))
	}
	select {
	case <-client.waitDone:
	case <-time.After(time.Second):
		t.Fatal("blocked-write peer was not reaped")
	}
	_ = client.Close()
}

func TestSoloLauncherReadsRenderedProcessOutputAsText(t *testing.T) {
	path := writeMCPPeer(t, `
import json, sys
for line in sys.stdin:
    msg=json.loads(line)
    if 'id' not in msg: continue
    if msg['method']=='initialize': result={}
    else:
        name=msg['params']['name']
        if name=='get_process_output':
            text='┃ Ask anything, / for commands, @ for context…'
        elif name=='get_process_status':
            text=json.dumps({'id':77,'name':'capstan-builder-018f0000-0000-7000-8000-000000000021','kind':'agent','status':'Running','agent_state':{'idle':True}})
        else:
            text='{}'
        result={'content':[{'type':'text','text':text}]}
    print(json.dumps({'jsonrpc':'2.0','id':msg['id'],'result':result}), flush=True)
`)
	client, err := NewStdioSoloClient(context.Background(), path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	launcher := NewSoloLauncher(client, time.Second)
	ref := SoloSessionRef{SoloProjectID: 8, SoloProcessID: 77, LaunchNonce: testNonce}
	got, err := launcher.output(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	want := "┃ Ask anything, / for commands, @ for context…"
	if got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestSoloLauncherTreatsRealMCPMissingStatusAsNotAliveOnly(t *testing.T) {
	for _, test := range []struct {
		name      string
		code      int
		wantError bool
	}{
		{name: "missing process", code: -32602},
		{name: "other method failure", code: -32000, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeMCPPeer(t, fmt.Sprintf(`
import json, sys
for line in sys.stdin:
    msg=json.loads(line)
    if 'id' not in msg: continue
    if msg['method']=='initialize': result={}
    elif msg['params']['name']=='get_process_status':
        print(json.dumps({'jsonrpc':'2.0','id':msg['id'],'error':{'code':%d,'message':'redacted-detail'}}), flush=True)
        continue
    else: result={'content':[{'type':'text','text':'{}'}]}
    print(json.dumps({'jsonrpc':'2.0','id':msg['id'],'result':result}), flush=True)
`, test.code))
			client, err := NewStdioSoloClient(context.Background(), path, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			ref, _ := (SoloSessionRef{SpokeID: 1, SoloProjectID: 32, SoloProcessID: 77, LaunchNonce: testNonce, Harness: "claude", AgentToolID: 3}).String()
			alive, err := NewSoloLauncher(client, time.Second).Alive(context.Background(), ref)
			if alive || (err != nil) != test.wantError || (err != nil && strings.Contains(err.Error(), "redacted-detail")) {
				t.Fatalf("Alive() = %v, %v", alive, err)
			}
		})
	}
}

func writeMCPPeer(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp-peer")
	if err := os.WriteFile(path, []byte("#!/usr/bin/env python3\n"+source), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
