package version

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

func Current() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	executable, err := os.Executable()
	if err != nil {
		return ""
	}

	out, err := exec.CommandContext(ctx, executable, "version").Output()
	if err != nil {
		return ""
	}

	return ParseCommandOutput(string(out))
}

func ParseCommandOutput(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}

	fields := strings.Fields(output)
	if len(fields) >= 2 && strings.EqualFold(fields[0], "v2node") {
		return fields[1]
	}

	return output
}
