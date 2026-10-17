package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/go-gitnest/GitNest/internal/app"
	"github.com/go-gitnest/GitNest/internal/config"
	"github.com/go-gitnest/GitNest/internal/identity"
	"golang.org/x/term"
)

const adminUsage = "usage: gitnest admin create"

// errNoInput is returned when the command runs without a terminal and without
// piped answers, instead of hanging or creating an account from empty fields.
var errNoInput = errors.New("no input available; run `gitnest admin create` in a terminal")

// runAdmin parses the admin subcommands and runs the interactive bootstrap.
//
// The password is never accepted as an argument: it is read from the terminal
// without echo, or from standard input when the command is driven by a script.
//
// No argument value is ever echoed in an error. An operator who mistakenly
// typed the password as an argument would otherwise see it printed back, and
// repeated in anything that captures the command's output; the usage line
// already tells them the form that is accepted.
func runAdmin(ctx context.Context, cfg config.Config, args []string, streams stdio) error {
	if len(args) == 0 {
		return errors.New(adminUsage)
	}
	if args[0] != "create" {
		return fmt.Errorf("%s (unknown subcommand)", adminUsage)
	}
	if len(args) > 1 {
		return fmt.Errorf("%s (this command takes no further arguments)", adminUsage)
	}

	user, err := createAdmin(ctx, cfg, newPrompter(os.Stdin, streams.err))
	if err != nil {
		return err
	}
	fmt.Fprintf(streams.out, "Created administrator %s <%s>.\n", user.Username, user.Email)
	return nil
}

// createAdmin prompts for the first administrator's credentials and creates the
// account through the application layer.
func createAdmin(ctx context.Context, cfg config.Config, prompts *prompter) (identity.User, error) {
	username, err := prompts.line("Username: ")
	if err != nil {
		return identity.User{}, err
	}
	email, err := prompts.line("Email: ")
	if err != nil {
		return identity.User{}, err
	}
	password, err := prompts.newPassword()
	if err != nil {
		return identity.User{}, err
	}

	user, err := app.CreateInitialAdmin(ctx, cfg, identity.CreateInitialAdminInput{
		Username: username,
		Email:    email,
		Password: password,
	})
	if err != nil {
		return identity.User{}, describeBootstrapError(err)
	}
	return user, nil
}

// describeBootstrapError turns the identity sentinels into advice an operator
// can act on, and passes anything else through unchanged.
func describeBootstrapError(err error) error {
	switch {
	case errors.Is(err, identity.ErrAdminExists):
		return errors.New("this instance already has an administrator; bootstrap runs once, " +
			"and further accounts are created by that administrator")
	case errors.Is(err, identity.ErrSchemaNotMigrated):
		return errors.New("the database schema is not migrated; run `gitnest migrate` first")
	case errors.Is(err, identity.ErrUsernameTaken):
		return errors.New("that username is already taken")
	case errors.Is(err, identity.ErrEmailTaken):
		return errors.New("that email address is already in use")
	default:
		return err
	}
}

// prompter reads credentials from a terminal or a pipe.
//
// Passwords are read without echo when standard input is a terminal. When it is
// a pipe the answers are read as ordinary lines, which is what lets a test drive
// the command; the password still never appears in the process arguments.
type prompter struct {
	in       io.Reader
	out      io.Writer
	file     *os.File
	terminal bool
	reader   *bufio.Reader
}

func newPrompter(in *os.File, out io.Writer) *prompter {
	return &prompter{in: in, out: out, file: in, terminal: term.IsTerminal(int(in.Fd()))}
}

// line reads one echoed line of input.
func (p *prompter) line(label string) (string, error) {
	if label != "" {
		if _, err := fmt.Fprint(p.out, label); err != nil {
			return "", err
		}
	}
	if p.terminal {
		return p.readTerminalLine()
	}
	if p.reader == nil {
		p.reader = bufio.NewReader(p.in)
	}
	value, err := p.reader.ReadString('\n')
	value = strings.TrimRight(value, "\r\n")
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if value == "" && errors.Is(err, io.EOF) {
		return "", errNoInput
	}
	return value, nil
}

// readTerminalLine reads one line byte by byte. It deliberately does not
// buffer, so a following password read cannot inherit input the terminal had
// already handed over.
func (p *prompter) readTerminalLine() (string, error) {
	var line []byte
	one := make([]byte, 1)
	for {
		n, err := p.in.Read(one)
		if n > 0 {
			switch one[0] {
			case '\n':
				return string(line), nil
			case '\r':
			default:
				line = append(line, one[0])
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return string(line), nil
			}
			if errors.Is(err, io.EOF) {
				return "", errNoInput
			}
			return "", err
		}
	}
}

// secret reads a password without echo on a terminal, and as an ordinary line
// from a pipe.
func (p *prompter) secret(label string) (string, error) {
	if !p.terminal {
		return p.line(label)
	}
	if _, err := fmt.Fprint(p.out, label); err != nil {
		return "", err
	}
	value, err := term.ReadPassword(int(p.file.Fd()))
	if _, writeErr := fmt.Fprintln(p.out); writeErr != nil && err == nil {
		return "", writeErr
	}
	if err != nil {
		return "", fmt.Errorf("read the password without echo: %w", err)
	}
	return string(value), nil
}

// newPassword asks for the password twice and retries a few times when the two
// entries differ, so a typo does not send the operator back to the start.
func (p *prompter) newPassword() (string, error) {
	const attempts = 3
	for attempt := 1; attempt <= attempts; attempt++ {
		password, err := p.secret("Password: ")
		if err != nil {
			return "", err
		}
		confirm, err := p.secret("Confirm password: ")
		if err != nil {
			return "", err
		}
		if password == confirm {
			return password, nil
		}
		if _, err := fmt.Fprintln(p.out, "Passwords did not match; try again."); err != nil {
			return "", err
		}
	}
	return "", errors.New("passwords did not match")
}
