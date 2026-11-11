package gatewayrun

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// Outcomes argparse of Python 3.14 gives the module entry parser for each argv, recorded at 80 columns without color
const runMainArgsRecorded = `[
{
"argv": [],
"config": null,
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"-h"
],
"config": null,
"done": true,
"code": 0,
"out": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\n\nIcebath Gateway - Multi-platform messaging\n\noptions:\n  -h, --help           show this help message and exit\n  --config, -c CONFIG  Path to gateway config file\n  --verbose, -v        Verbose output\n",
"err": ""
},
{
"argv": [
"--help"
],
"config": null,
"done": true,
"code": 0,
"out": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\n\nIcebath Gateway - Multi-platform messaging\n\noptions:\n  -h, --help           show this help message and exit\n  --config, -c CONFIG  Path to gateway config file\n  --verbose, -v        Verbose output\n",
"err": ""
},
{
"argv": [
"-v"
],
"config": null,
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"--verbose"
],
"config": null,
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"-c",
"p"
],
"config": "p",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"--config",
"p"
],
"config": "p",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"--config=p"
],
"config": "p",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"-cp"
],
"config": "p",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"-c=p"
],
"config": "p",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"--conf",
"p"
],
"config": "p",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"--c=p"
],
"config": "p",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"--v"
],
"config": null,
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"--verb=1"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: argument --verbose/-v: ignored explicit argument '1'\n"
},
{
"argv": [
"-vc",
"p"
],
"config": "p",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"-vcp"
],
"config": "p",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"-vh"
],
"config": null,
"done": true,
"code": 0,
"out": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\n\nIcebath Gateway - Multi-platform messaging\n\noptions:\n  -h, --help           show this help message and exit\n  --config, -c CONFIG  Path to gateway config file\n  --verbose, -v        Verbose output\n",
"err": ""
},
{
"argv": [
"-hv"
],
"config": null,
"done": true,
"code": 0,
"out": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\n\nIcebath Gateway - Multi-platform messaging\n\noptions:\n  -h, --help           show this help message and exit\n  --config, -c CONFIG  Path to gateway config file\n  --verbose, -v        Verbose output\n",
"err": ""
},
{
"argv": [
"-vx"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: unrecognized arguments: -x\n"
},
{
"argv": [
"-v=1"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: argument --verbose/-v: ignored explicit argument '1'\n"
},
{
"argv": [
"-c"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: argument --config/-c: expected one argument\n"
},
{
"argv": [
"--config"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: argument --config/-c: expected one argument\n"
},
{
"argv": [
"-c",
"--",
"p"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: argument --config/-c: expected one argument\n"
},
{
"argv": [
"-c",
"-v"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: argument --config/-c: expected one argument\n"
},
{
"argv": [
"x"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: unrecognized arguments: x\n"
},
{
"argv": [
"--",
"x"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: unrecognized arguments: -- x\n"
},
{
"argv": [
"--"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: unrecognized arguments: --\n"
},
{
"argv": [
"--bogus"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: unrecognized arguments: --bogus\n"
},
{
"argv": [
"--bogus",
"-h"
],
"config": null,
"done": true,
"code": 0,
"out": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\n\nIcebath Gateway - Multi-platform messaging\n\noptions:\n  -h, --help           show this help message and exit\n  --config, -c CONFIG  Path to gateway config file\n  --verbose, -v        Verbose output\n",
"err": ""
},
{
"argv": [
"-c",
"-1"
],
"config": "-1",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"-c",
"a b"
],
"config": "a b",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"-c",
""
],
"config": "",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"-vv"
],
"config": null,
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"-vvc",
"p"
],
"config": "p",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"-x"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: unrecognized arguments: -x\n"
},
{
"argv": [
"-c",
"p",
"-c",
"q"
],
"config": "q",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"--=x"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: ambiguous option: --=x could match --help, --config, --verbose\n"
},
{
"argv": [
"-"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: unrecognized arguments: -\n"
},
{
"argv": [
"-v-"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: argument --verbose/-v: ignored explicit argument '-'\n"
},
{
"argv": [
"-c",
"-"
],
"config": "-",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"-h=1"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: argument -h/--help: ignored explicit argument '1'\n"
},
{
"argv": [
"--help=1"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: argument -h/--help: ignored explicit argument '1'\n"
},
{
"argv": [
"-vc"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: argument --config/-c: expected one argument\n"
},
{
"argv": [
"-vc=p"
],
"config": "p",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"-cv"
],
"config": "v",
"done": false,
"code": 0,
"out": "",
"err": ""
},
{
"argv": [
"--verbose",
"--config",
"-x"
],
"config": null,
"done": true,
"code": 2,
"out": "",
"err": "usage: icebath __icebath_gateway_run__ [-h] [--config CONFIG] [--verbose]\nicebath __icebath_gateway_run__: error: argument --config/-c: expected one argument\n"
}
]`

// The module entry parser reads every recorded argv the way argparse reads it for gateway/run.py main
func TestParseRunMainArgsMatchesArgparse(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	t.Setenv("NO_COLOR", "1")
	var rows []struct {
		Argv   []string `json:"argv"`
		Config *string  `json:"config"`
		Done   bool     `json:"done"`
		Code   int      `json:"code"`
		Out    string   `json:"out"`
		Err    string   `json:"err"`
	}
	if err := json.Unmarshal([]byte(runMainArgsRecorded), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no recorded rows")
	}
	for _, row := range rows {
		var stdout, stderr bytes.Buffer
		config, code, done := parseRunMainArgs(row.Argv, &stdout, &stderr)
		if done != row.Done || code != row.Code || !reflect.DeepEqual(config, row.Config) || stdout.String() != row.Out || stderr.String() != row.Err {
			got := "<nil>"
			if config != nil {
				got = *config
			}
			t.Errorf("argv %q: config=%s code=%d done=%v\nstdout %q\nstderr %q\nwant code=%d done=%v\nstdout %q\nstderr %q",
				row.Argv, got, code, done, stdout.String(), stderr.String(), row.Code, row.Done, row.Out, row.Err)
		}
	}
}
