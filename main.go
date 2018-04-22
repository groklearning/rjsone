package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/ghodss/yaml"
	jsone "github.com/taskcluster/json-e"
	jsone_interpreter "github.com/taskcluster/json-e/interpreter"
)

const description = `rjsone is a simple wrapper around the JSON-e templating language.

See: https://taskcluster.github.io/json-e/

Context is usually provided by a list of arguments. By default,
these are interpreted as files. However, if the 'filename' begins with
a '+', the rest of the argument is interpreted as a raw string.
Data is loaded as YAML/JSON by default and merged into the
main context. You can specify a particular key to load a JSON
file into using keyname:filename.yaml; if you specify two colons
(i.e. keyname::filename.yaml) it will load it as a raw string.
When duplicate keys are found, later entries replace earlier
at the top level only (no multi-level merging).

You can also use keyname:.. (or keyname::..) to indicate that subsequent
entries without keys should be loaded as a list element into that key. If you
instead use 'keyname:...', metadata information is loaded as well
(filename, basename, content).

For complex applications, single argument functions can be added by prefixing
the filename with a '-' (or a '--' for raw string input). For example:

    b64decode::--'base64 -d'
	
This adds a base64 decode function to the context which accepts a string
as input and outputs a string. Conversely, if you use :-, this accepts
JSON as input and outputs JSON or YAML.
`

type arguments struct {
	yaml         bool
	indentation  int
	templateFile string
	verbose      bool
	contexts     []string
}

func main() {
	var args arguments
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), description)
		fmt.Fprintf(flag.CommandLine.Output(), "\nUsage: %s [options] [[key:[:]]contextfile ...]\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprint(flag.CommandLine.Output(), "\n")
	}
	flag.StringVar(&args.templateFile, "t", "-", "file to use for template (- is stdin)")
	flag.BoolVar(&args.yaml, "y", false, "output YAML rather than JSON (always reads YAML/JSON)")
	flag.BoolVar(&args.verbose, "v", false, "show information about processing on stderr")
	flag.IntVar(&args.indentation, "i", 2, "indentation of JSON output; 0 means no pretty-printing")
	flag.Parse()

	args.contexts = flag.Args()

	if err := run(args); err != nil {
		fmt.Fprintf(flag.CommandLine.Output(), "Fatal error: %s\n", err)
		os.Exit(2)
	}
}

func run(args arguments) error {
	l := log.New(os.Stderr, "", 0)
	template, err := readDataArgument(args.templateFile, false)
	if err != nil {
		return err
	}

	context, err := loadContext(args.contexts)
	if err != nil {
		return err
	}

	if args.verbose {
		l.Println("Calculated context:")
		output, err := yaml.Marshal(context)
		if err != nil {
			return err
		}
		l.Println(string(output))
	}

	output, err := jsone.Render(template, context)
	if err != nil {
		return err
	}

	var byteOutput []byte
	if args.yaml {
		byteOutput, err = yaml.Marshal(output)
	} else if args.indentation == 0 {
		byteOutput, err = json.Marshal(output)
	} else {
		byteOutput, err = json.MarshalIndent(output, "", strings.Repeat(" ", args.indentation))
		// MarshalIndent, sadly, doesn't add a newline at the end. Which I think it should.
		byteOutput = append(byteOutput, 0x0a)
	}

	if err != nil {
		return err
	}

	_, err = os.Stdout.Write(byteOutput)
	return err
}

func loadContext(contextOps []string) (map[string]interface{}, error) {
	context := make(map[string]interface{})

	var currentContextList struct {
		raw      bool
		key      string
		metadata bool
	}

	for _, contextOp := range contextOps {
		splitContextOp := strings.SplitN(contextOp, ":", 2)
		if len(splitContextOp) < 2 { // i.e. we just have a file to load
			entry := splitContextOp[0]
			if currentContextList.key == "" { // we're not in a list - just load it in!
				data, err := readDataArgument(entry, false)
				if err != nil {
					return nil, err
				}
				mapData, ok := data.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("direct merge of %q failed - not an object, prefix with a key", entry)
				}
				for k, v := range mapData {
					context[k] = v
				}
			} else { // ah, we're in a list; we should append it to the right key
				data, err := readDataArgument(entry, currentContextList.raw)
				if err != nil {
					return nil, err
				}
				if currentContextList.metadata {
					contextData := map[string]interface{}{
						"content": data,
					}

					// hack...
					basename := path.Base(entry)
					if !strings.HasPrefix(entry, "+") {
						contextData["filename"] = entry
						contextData["basename"] = basename
						contextData["name"] = strings.TrimSuffix(basename, filepath.Ext(basename))
					}
					data = contextData
				}
				context[currentContextList.key] = append(context[currentContextList.key].([]interface{}), data)
			}
		} else { // we have a key
			key := splitContextOp[0]
			if key == "" {
				return nil, fmt.Errorf("must specify key before ':' in %q", contextOp)
			}
			raw := strings.HasPrefix(splitContextOp[1], ":")
			var entry string
			if raw {
				entry = splitContextOp[1][1:]
			} else {
				entry = splitContextOp[1]
			}
			if entry == "" {
				return nil, fmt.Errorf("must specify entry or ellipsis after ':' in %q", contextOp)
			}

			if entry == ".." || entry == "..." { // we have a list to follow - switch mode!
				if _, ok := context[key].([]interface{}); !ok {
					context[key] = make([]interface{}, 0)
				}
				currentContextList.key = key
				currentContextList.raw = raw
				currentContextList.metadata = entry == "..."
			} else { // otherwise, we end any existing list and set this directly
				currentContextList.key = ""
				data, err := readDataArgument(entry, raw)
				if err != nil {
					return nil, err
				}
				context[key] = data
			}
		}
	}

	return context, nil
}

func readDataArgument(entry string, raw bool) (interface{}, error) {
	var data []byte
	var err error
	if strings.HasPrefix(entry, "+") {
		data = []byte(entry[1:])
	} else if entry == "-" {
		data, err = ioutil.ReadAll(os.Stdin)
	} else if strings.HasPrefix(entry, "-") {
		entry := entry[1:]
		if strings.HasPrefix(entry, "-") {
			return buildFunction(entry[1:], raw, true), nil
		} else {
			return buildFunction(entry, raw, false), nil
		}
	} else {
		data, err = ioutil.ReadFile(entry)
	}

	if err != nil {
		return nil, err
	}

	if raw {
		return string(data), nil
	}

	var o interface{}
	err = yaml.Unmarshal(data, &o)
	return o, err
}

// Builds a function out of a command that does stdin/stdout
func buildFunction(commandString string, rawOutput, rawInput bool) interface{} {
	var f interface{}
	commandArray := strings.Split(commandString, " ")

	if rawInput && rawOutput {
		f = func(s string) (string, error) {
			command := exec.Command(commandArray[0], commandArray[1:]...)
			command.Stderr = os.Stderr
			command.Stdin = bytes.NewReader([]byte(s))
			stdoutBytes, err := command.Output()
			if err != nil {
				return "", err
			}
			return string(stdoutBytes), nil
		}
	} else if rawInput {
		f = func(s string) (interface{}, error) {
			command := exec.Command(commandArray[0], commandArray[1:]...)
			command.Stderr = os.Stderr
			command.Stdin = bytes.NewReader([]byte(s))
			stdoutBytes, err := command.Output()
			if err != nil {
				return nil, err
			}

			var o interface{}
			err = yaml.Unmarshal(stdoutBytes, &o)
			if err != nil {
				return nil, err
			}
			return o, nil
		}
	} else if rawOutput {
		f = func(v interface{}) (string, error) {
			jsonBytes, err := json.Marshal(v)
			if err != nil {
				return "", err
			}

			command := exec.Command(commandArray[0], commandArray[1:]...)
			command.Stderr = os.Stderr
			command.Stdin = bytes.NewReader(jsonBytes)
			stdoutBytes, err := command.Output()
			return string(stdoutBytes), err
		}
	} else {
		f = func(v interface{}) (interface{}, error) {
			jsonBytes, err := json.Marshal(v)
			if err != nil {
				return "", err
			}

			command := exec.Command(commandArray[0], commandArray[1:]...)
			command.Stderr = os.Stderr
			command.Stdin = bytes.NewReader(jsonBytes)
			stdoutBytes, err := command.Output()
			if err != nil {
				return nil, err
			}

			var o interface{}
			err = yaml.Unmarshal(stdoutBytes, &o)
			if err != nil {
				return nil, err
			}
			return v, nil
		}
	}

	return jsone_interpreter.WrapFunction(f)
}
