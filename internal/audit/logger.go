package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type AuditEntry struct {
	Timestamp time.Time       `json:"timestamp"`
	Direction string          `json:"direction"` // "request" or "response"
	Method    string          `json:"method,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

var (
	logFile *os.File
	mu      sync.Mutex
)

// InitLogger opens the audit.jsonl log file
func InitLogger() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	logDir := filepath.Join(home, ".config", "mcp-manager")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return err
	}

	logPath := filepath.Join(logDir, "audit.jsonl")
	logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	return nil
}

// LogEntry appends an entry to the log file in JSON Lines format
func LogEntry(direction string, method string, payload []byte) {
	mu.Lock()
	defer mu.Unlock()

	if logFile == nil {
		return
	}

	entry := AuditEntry{
		Timestamp: time.Now(),
		Direction: direction,
		Method:    method,
		Payload:   json.RawMessage(payload),
	}

	bytes, err := json.Marshal(entry)
	if err == nil {
		_, _ = logFile.Write(append(bytes, '\n'))
	}

	// Rotate log file if it exceeds 5MB to prevent disk space exhaustion and slow loads
	if stat, err := logFile.Stat(); err == nil && stat.Size() > 5*1024*1024 {
		_ = logFile.Close()

		home, err := os.UserHomeDir()
		if err == nil {
			logDir := filepath.Join(home, ".config", "mcp-manager")
			logPath := filepath.Join(logDir, "audit.jsonl")
			oldPath := filepath.Join(logDir, "audit.old.jsonl")

			// Remove old backup and rename current to old
			_ = os.Remove(oldPath)
			_ = os.Rename(logPath, oldPath)

			// Re-open fresh log file
			logFile, _ = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		}
	}
}

// GetAuditLogPath returns the path to audit.jsonl
func GetAuditLogPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "mcp-manager", "audit.jsonl")
}

// ReadLastEntries retrieves the last N log entries
func ReadLastEntries(n int) ([]string, error) {
	logPath := GetAuditLogPath()
	file, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	// Allow lines up to 1MB (e.g. large JSON tool responses)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		// Return whatever lines we successfully read, plus log or return the error.
		// Since this is for UI, returning the lines read is best, but we should log/report the scanning error if possible.
		return lines, err
	}

	if len(lines) == 0 {
		return []string{}, nil
	}

	start := len(lines) - n
	if start < 0 {
		start = 0
	}
	return lines[start:], nil
}
