package main

import (
	"bytes"
	"errors"
	"fmt"
	"gopkg.in/yaml.v2"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gocarina/gocsv"
	"github.com/mkideal/cli"
	clix "github.com/mkideal/cli/ext"
)

type argT struct {
	PasswordStore string       `cli:"password-store" dft:"$HOME/.password-store" usage:"password store location"`
	Help          bool         `cli:"!h,help" usage:"show help"`
	Output        *clix.Writer `cli:"o,output" usage:"output file or stdout"`
}

type mapString struct {
	content map[string]string
}

func (m *mapString) MarshalCSV() (string, error) {
	var builder strings.Builder
	for k, v := range m.content {
		builder.WriteString(fmt.Sprintf("%s: %s\n", k, v))
	}
	return builder.String(), nil
}

type entry struct {
	Folder        string    `csv:"folder"`
	Favorite      int       `csv:"favorite"`
	Type          string    `csv:"type"`
	Name          string    `csv:"name"`
	Notes         string    `csv:"notes"`
	Fields        mapString `csv:"fields"`
	LoginURI      string    `csv:"login_uri"`
	LoginUsername string    `csv:"login_username"`
	LoginPassword string    `csv:"login_password"`
	LoginTOTP     string    `csv:"login_totp"`
}

func pop(m map[string]string, key string) string {
	v, ok := m[key]
	if ok {
		delete(m, key)
	}
	return v
}

func buildEntry(fname string, out []byte) entry {
	folder, name := filepath.Split(fname)
	lines := strings.Split(string(out), "\n")
	password := lines[0]

	content := lines[1:]
	if len(lines) > 1 && (lines[1] == "--" || lines[1] == "---") {
		content = lines[2:]
	}

	fields := make(map[string]string)
	err := yaml.Unmarshal([]byte(strings.Join(content, "\n")), &fields)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not parse content of password %s: %s\n", fname, err)
	}

	username, has := fields["login"]
	if !has {
		username = fields["username"]
	}
	pop(fields, "login")
	pop(fields, "username")

	url := pop(fields, "url")
	if url == "" {
		url = pop(fields, "http")
	} else {
		delete(fields, "http")
	}
	totp := pop(fields, "totp")
	entryType := "login"
	if totp != "" {
		entryType = "totp"
	}

	// Handle passwords that are stored on the 'root' of the directory.
	if len(folder) == 1 {
		folder = "/"
	} else {
		folder = folder[1 : len(folder)-1]
	}

	return entry{
		Folder:        folder,
		Name:          name[:len(name)-4],
		Type:          entryType,
		LoginURI:      url,
		Fields:        mapString{fields},
		LoginUsername: username,
		LoginPassword: password,
		LoginTOTP:     totp,
	}
}

func decrypt(basepath string, done <-chan struct{}, paths <-chan string, resultc chan<- *entry) error {
	for path := range paths {
		fname := path[len(basepath):]
		out, err := exec.Command("gpg", "-qd", path).Output()
		if err != nil {
			fmt.Printf("Error while decrypting entry %s: %s", fname, err)
		}

		entry := buildEntry(fname, out)
		select {
		case resultc <- &entry:
		case <-done:
			return errors.New("Operation aborted")
		}
	}
	return nil
}

func parse(done <-chan struct{}, basepath string) (<-chan *entry, <-chan error) {
	paths, errc := walkFiles(done, basepath)
	c := make(chan *entry)
	go func() {
		decrypt(basepath, done, paths, c)
		close(c)
	}()
	return c, errc
}

func walkFiles(done <-chan struct{}, root string) (<-chan string, <-chan error) {
	paths := make(chan string)
	errc := make(chan error, 1)
	go func() {
		defer close(paths)
		errc <- filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			if !strings.HasSuffix(path, "gpg") {
				return nil
			}
			select {
			case paths <- path:
			case <-done:
				return errors.New("walk canceled")
			}
			return nil
		})
	}()
	return paths, errc
}

func writeCSV(out io.Writer, entries <-chan *entry) error {
	outChan := make(chan interface{})
	// map channel type to internal one
	go func() {
		for e := range entries {
			select {
			case outChan <- e:
			}
		}
		close(outChan)
	}()

	err := gocsv.MarshalChan(outChan, gocsv.DefaultCSVWriter(out))
	if err != nil {
		return err
	}
	return nil
}

func unlockGPGKey() error {
	// unlocking gpg key before the start
	cmd := exec.Command("gpg2", "-aso", "-")
	cmd.Stdin = bytes.NewBufferString("1234")
	return cmd.Run()
}

func run(ctx *cli.Context) error {
	argv := ctx.Argv().(*argT)

	err := unlockGPGKey()
	if err != nil {
		return fmt.Errorf("failed to unlock gpg key: %v", err)
	}

	done := make(chan struct{})
	entries, errc := parse(done, argv.PasswordStore)

	err = writeCSV(argv.Output, entries)
	if err != nil {
		return err
	}

	if err := <-errc; err != nil {
		return err
	}
	return nil
}

func (argv *argT) AutoHelp() bool {
	return argv.Help
}

func main() {
	code := cli.Run(new(argT), run)
	os.Exit(code)
}
