package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func portableCompileFolder(workspace string, args []string) (string, error) {
	if len(args) != 2 || args[0] != "quickstart" || args[1] == "" || strings.HasPrefix(args[1], "-") {
		return "", errors.New("this portable workspace accepts only quickstart and one local folder; use the command line for other workflows")
	}
	if err := validateWorkbenchArgs(args); err != nil {
		return "", err
	}
	folder := args[1]
	if !filepath.IsAbs(folder) {
		folder = filepath.Join(workspace, folder)
	}
	info, err := os.Stat(folder)
	if err != nil || !info.IsDir() {
		return "", errors.New("choose an existing local folder")
	}
	return filepath.Clean(folder), nil
}

// The portable workspace uses the same bounded deterministic compiler as the
// native CLI, in one cancellable job. There is no child-process tree to claim
// control over; the slot remains occupied until the trusted callback returns.
func (workbench *Workbench) runFolderJob(ctx context.Context, id string, args []string, releaseSlot func()) {
	folder, err := portableCompileFolder(workbench.workspace, args)
	if err == nil {
		err = workbench.compileFolder(ctx, folder)
	}
	if ctx.Err() != nil {
		releaseSlot()
		workbench.finishJobFromContext(id, nil, nil)
		return
	}
	if err != nil {
		releaseSlot()
		workbench.finishJob(id, "failed", 1, "", false, err.Error())
		return
	}
	if err = workbench.activateCompletedQuickstart(id, args); err != nil {
		releaseSlot()
		workbench.finishJob(id, "failed", 1, "", false, "compiled atlas was not activated: "+err.Error())
		return
	}
	releaseSlot()
	workbench.finishJob(id, "succeeded", 0, "Folder compiled and verified.", false, "")
}
