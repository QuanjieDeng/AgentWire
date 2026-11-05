package install

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// One shared reader: a fresh bufio.Reader per prompt would consume and discard
// the next line when input is piped.
var stdin = bufio.NewReader(os.Stdin)

func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	line, err := stdin.ReadString('\n')
	fmt.Fprintln(os.Stderr)
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
