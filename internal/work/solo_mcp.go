package work

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

const maxMCPMessageBytes = 1 << 20

type rpcMessage struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// StdioSoloClient is a bounded JSON-RPC client for Solo's stdio MCP process.
type StdioSoloClient struct {
	command   *exec.Cmd
	stdin     io.WriteCloser
	messages  chan rpcMessage
	readError chan error
	timeout   time.Duration
	mu        sync.Mutex
	nextID    int64
	closed    bool
}

// NewStdioSoloClient starts and identifies an external Capstan MCP actor.
func NewStdioSoloClient(ctx context.Context, path string, timeout time.Duration) (*StdioSoloClient, error) {
	if path == "" || timeout <= 0 {
		return nil, errors.New("solo MCP configuration is invalid")
	}
	command := exec.CommandContext(ctx, path)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, errors.New("solo MCP stdin could not be opened")
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, errors.New("solo MCP stdout could not be opened")
	}
	if err := command.Start(); err != nil {
		return nil, errors.New("solo MCP process could not be started")
	}
	client := &StdioSoloClient{
		command: command, stdin: stdin, messages: make(chan rpcMessage, 8), readError: make(chan error, 1), timeout: timeout,
	}
	go client.read(stdout)
	var initialized json.RawMessage
	if err := client.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "capstan-work", "version": "1"},
	}, &initialized); err != nil {
		_ = client.Close()
		return nil, err
	}
	if err := client.notify("notifications/initialized", map[string]any{}); err != nil {
		_ = client.Close()
		return nil, err
	}
	if err := client.Call(ctx, "identify_session", map[string]any{
		"external": map[string]string{"name": "capstan-work", "agent_id": "capstan-work"},
	}, nil); err != nil {
		_ = client.Close()
		return nil, err
	}

	return client, nil
}

func (c *StdioSoloClient) read(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), maxMCPMessageBytes)
	for scanner.Scan() {
		var message rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			select {
			case c.readError <- errors.New("solo MCP returned invalid JSON-RPC"):
			default:
			}
			return
		}
		if message.ID != 0 {
			c.messages <- message
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	select {
	case c.readError <- err:
	default:
	}
}

func (c *StdioSoloClient) rpc(ctx context.Context, method string, params any, target any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("solo MCP client is closed")
	}
	c.nextID++
	id := c.nextID
	request := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	data, err := json.Marshal(request)
	if err != nil {
		return errors.New("solo MCP request could not be encoded")
	}
	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		return errors.New("solo MCP request could not be written")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	for {
		select {
		case <-callCtx.Done():
			_ = c.command.Process.Kill()
			return errors.New("solo MCP request timed out")
		case <-c.readError:
			return errors.New("solo MCP closed before replying")
		case message := <-c.messages:
			if message.ID != id {
				continue
			}
			if message.Error != nil {
				return fmt.Errorf("solo MCP method failed (%d)", message.Error.Code)
			}
			if target == nil {
				return nil
			}
			if err := json.Unmarshal(message.Result, target); err != nil {
				return errors.New("solo MCP result has an invalid shape")
			}
			return nil
		}
	}
}

func (c *StdioSoloClient) notify(method string, params any) error {
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return errors.New("solo MCP notification could not be encoded")
	}
	_, err = c.stdin.Write(append(data, '\n'))
	if err != nil {
		return errors.New("solo MCP notification could not be written")
	}
	return nil
}

// Call invokes one Solo MCP tool and decodes its text result as JSON.
func (c *StdioSoloClient) Call(ctx context.Context, name string, arguments any, target any) error {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := c.rpc(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments}, &result); err != nil {
		return err
	}
	if result.IsError {
		return errors.New("solo MCP tool call failed")
	}
	text := ""
	for _, content := range result.Content {
		if content.Type == "text" {
			text += content.Text
		}
	}
	if target == nil {
		return nil
	}
	if raw, ok := target.(*string); ok {
		*raw = text
		return nil
	}
	if len(text) > maxMCPMessageBytes || json.Unmarshal([]byte(text), target) != nil {
		return errors.New("solo MCP tool result has an invalid shape")
	}
	return nil
}

// Close terminates the disposable stdio MCP process.
func (c *StdioSoloClient) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	_ = c.stdin.Close()
	c.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- c.command.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(2 * time.Second):
		_ = c.command.Process.Kill()
		<-done
		return nil
	}
}
