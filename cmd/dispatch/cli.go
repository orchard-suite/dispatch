package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const usage = `dispatch                 run the desk
dispatch export          print the desk's routing as YAML
dispatch apply FILE      make the desk's routing what FILE says ("-" is stdin)

export and apply call the desk at DISPATCH_URL (its API, such as
https://dispatch.example.org) with the token in DISPATCH_TOKEN.`

// cli runs a subcommand and says whether there was one.
func cli(args []string, stdout io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "export":
		return true, call(http.MethodGet, nil, stdout)
	case "apply":
		if len(args) != 2 {
			return true, errors.New("apply takes one file")
		}
		var in io.Reader = os.Stdin
		if args[1] != "-" {
			f, err := os.Open(args[1])
			if err != nil {
				return true, err
			}
			defer f.Close()
			in = f
		}
		return true, call(http.MethodPut, in, stdout)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, usage)
		return true, nil
	}
	return true, fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
}

func call(method string, body io.Reader, stdout io.Writer) error {
	base, token := strings.TrimRight(os.Getenv("DISPATCH_URL"), "/"), os.Getenv("DISPATCH_TOKEN")
	if base == "" || token == "" {
		return errors.New("set DISPATCH_URL and DISPATCH_TOKEN")
	}
	req, err := http.NewRequest(method, base+"/v1/config", body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/yaml")
	res, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		said, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("the desk answered %d: %s", res.StatusCode, strings.TrimSpace(string(said)))
	}
	_, err = io.Copy(stdout, res.Body)
	return err
}
