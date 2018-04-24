package shell

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"bytes"
	"encoding/base64"
	"github.com/armon/circbuf"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/mitchellh/go-linereader"
	"text/template"
	"os"
)

func resourceGenericShellCreate(d *schema.ResourceData, meta interface{}) error {
	config := meta.(*Config)

	command, err := interpolateCommand(
		mergeCommands(config, config.CommandPrefix, config.CreateCommand),
		getContext(d, "create"))
	if err != nil {
		return err
	}
	writeLog(config, "DEBUG", "creating resource")
	_, err = runCommand(config, command)
	if err != nil {
		return err
	}

	d.SetId(hash(command))
	writeLog(config, "DEBUG", "created generic resource: %s", d.Id())

	return resourceShellRead(d, meta)
}

func getOutputsText(config *Config, output string, prefix string) map[string]string {
	outputs := make(map[string]string)
	split := strings.Split(output, "\n")
	for _, varline := range split {
		writeLog(config, "DEBUG", "reading output line: %s", varline)

		if varline == "" {
			continue
		}

		if prefix != "" {
			if !strings.HasPrefix(varline, prefix) {
				writeLog(config, "INFO", "ignoring line without prefix `%s`: \"%s\"", prefix, varline)
				continue
			}
			varline = strings.TrimPrefix(varline, prefix)
		}

		pos := strings.Index(varline, "=")
		if pos == -1 {
			writeLog(config, "INFO", "ignoring line without equal sign: \"%s\"", varline)
			continue
		}

		key := varline[:pos]
		value := varline[pos+1:]
		writeLog(config, "DEBUG", "read output entry (raw): \"%s\" = \"%s\"", key, value)
		outputs[key] = value
	}
	return outputs
}

func getOutputsBase64(config *Config, output, prefix string) map[string]string {
	outputs := make(map[string]string)
	for key, value := range getOutputsText(config, output, prefix) {
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			writeLog(config, "WARN", "error decoding %s", err)
			continue
		}
		writeLog(config, "DEBUG", "read output entry (decoded): \"%s\" = \"%s\" (%s)", key, decoded, value)
		outputs[key] = string(decoded[:])
	}
	return outputs
}

func resourceShellRead(d *schema.ResourceData, meta interface{}) error {
	config := meta.(*Config)

	command, err := interpolateCommand(
		mergeCommands(config, config.CommandPrefix, config.ReadCommand),
		getContext(d, "read"))
	if err != nil {
		return err
	}
	writeLog(config, "DEBUG", "reading resource")
	stdout, err := runCommand(config, command)
	if err != nil {
		writeLog(config, "INFO", "command returned error (%s), marking resource deleted: %s", err, stdout)
		if config.ReadDeleteOnFailure {
			d.SetId("")
			return nil
		}
		return err
	}
	var outputs map[string]string

	switch config.ReadFormat {
	case "base64":
		outputs = getOutputsBase64(config, stdout, config.ReadLinePrefix)
	default:
		fallthrough
	case "raw":
		outputs = getOutputsText(config, stdout, config.ReadLinePrefix)
	}
	d.Set("output", outputs)

	return nil
}

func resourceGenericShellUpdate(d *schema.ResourceData, meta interface{}) error {
	config := meta.(*Config)

	ctx := getContext(d, "update")
	deleteCommand, _ := interpolateCommand(
		mergeCommands(config, config.CommandPrefix, config.DeleteCommand),
		mergeMaps(ctx, map[string]interface{}{"cur": ctx["old"]}))
	createCommand, _ := interpolateCommand(mergeCommands(config, config.CommandPrefix, config.CreateCommand), ctx)
	command, err := interpolateCommand(
		mergeCommands(config, config.CommandPrefix, config.UpdateCommand),
		mergeMaps(ctx, map[string]interface{}{
			"delete_command": deleteCommand,
			"create_command": createCommand,
		}))
	if err != nil {
		return err
	}
	writeLog(config, "DEBUG", "updating resource: %s", command)
	_, err = runCommand(config, command)
	if err != nil {
		writeLog(config, "WARN", "update command returned error: %s", err)
		return nil
	}

	return resourceShellRead(d, meta)
}

func resourceGenericShellExists(d *schema.ResourceData, meta interface{}) (bool, error) {
	config := meta.(*Config)

	command, err := interpolateCommand(
		mergeCommands(config, config.CommandPrefix, config.ExistsCommand),
		getContext(d, "exists"))
	if err != nil {
		return false, err
	}
	writeLog(config, "DEBUG", "resource exists")
	stdout, err := runCommand(config, command)
	if err != nil {
		writeLog(config, "WARN", "command returned error %s", err)
	}
	return stdout == "true", err
}

func resourceGenericShellDelete(d *schema.ResourceData, meta interface{}) error {
	config := meta.(*Config)

	command, err := interpolateCommand(
		mergeCommands(config, config.CommandPrefix, config.DeleteCommand),
		getContext(d, "delete"))
	if err != nil {
		return err
	}
	writeLog(config, "DEBUG", "reading resource")
	_, err = runCommand(config, command)
	if err != nil {
		return err
	}

	d.SetId("")
	return nil
}

func getContext(d *schema.ResourceData, operation string) map[string]interface{} {
	return getContextFull(d, operation, false)
}

func getContextFull(d *schema.ResourceData, operation string, oldIsCurrent bool) map[string]interface{} {
	o, n := d.GetChange("context")
	cur := n
	if oldIsCurrent {
		cur = o
	}
	return map[string]interface{}{
		"operation": operation,
		"old":       o,
		"new":       n,
		"cur":       cur,
	}
}

func mergeMaps(maps ...map[string]interface{}) map[string]interface{} {
	ctx := map[string]interface{}{}
	for _, m := range maps {
		for k, v := range m {
			ctx[k] = v
		}
	}
	return ctx
}

func interpolateCommand(command string, context map[string]interface{}) (string, error) {
	t := template.Must(template.New("command").Parse(command))
	var buf bytes.Buffer
	err := t.Execute(&buf, context)
	return buf.String(), err
}

func runCommand(config *Config, commands ...string) (string, error) {
	// Setup the command
	interpreter := config.Interpreter[0]
	command := mergeCommands(config, commands...)
	args := append(config.Interpreter[1:], command)
	cmd := exec.Command(interpreter, args...)

	// Setup the reader that will read the output from the command.
	// We use an os.Pipe so that the *os.File can be passed directly to the
	// process, and not rely on goroutines copying the data which may block.
	// See golang.org/issue/18874
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", fmt.Errorf("failed to initialize pipe for output: %s", err)
	}

	stdout, _ := circbuf.NewBuffer(config.BufferSize)
	output, _ := circbuf.NewBuffer(8 * 1024)

	cmd.Stdout = io.MultiWriter(pw, stdout)
	cmd.Stderr = pw

	// Write everything we read from the pipe to the output buffer too
	tee := io.TeeReader(pr, output)

	// copy the teed output to the logger
	copyDoneCh := make(chan struct{})
	go copyOutput(config, tee, copyDoneCh)

	// Output what we're about to run
	writeLog(config, "DEBUG", "executing: %s %s", interpreter, strings.Join(args, " "))

	// Start the command
	err = cmd.Start()
	if err == nil {
		err = cmd.Wait()
	}

	// Close the write-end of the pipe so that the goroutine mirroring output
	// ends properly.
	pw.Close()

	select {
	case <-copyDoneCh:
	}
	if err != nil {
		return stdout.String(), fmt.Errorf("error running command '%s': %v. Output: %s",
			command, err, output.Bytes())
	}
	return stdout.String(), nil
}

func mergeCommands(config *Config, commands ...string) string {
	return strings.Join(commands, config.CommandSeparator)
}

func hash(s string) string {
	sha := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sha[:])
}

func copyOutput(config *Config, r io.Reader, doneCh chan<- struct{}) {
	defer close(doneCh)
	lr := linereader.New(r)
	for line := range lr.Ch {
		if config.LogOutput{
			writeLog(config, "", line)
		}
	}
}

func writeLog(config *Config, level, format string, v ...interface{}) {
	output := fmt.Sprintf(format, v...)
	if level == "" {
		config.Logger.Output(2, output)
		return
	}

	config.Logger.Output(2, strings.Join([]string{
		fmt.Sprintf("[%s]", level),
		fmt.Sprintf(format, v...),
	}, " "))
}
