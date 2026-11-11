//go:build !windows
// +build !windows

package download

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/rs/zerolog"
)

func TestScript_TriggerOnDone_MoveFile(t *testing.T) {
	tmpDir := t.TempDir()
	downloadDir := filepath.Join(tmpDir, "downloads")
	destDir := filepath.Join(tmpDir, "moved")
	task, taskPath := createDownloadDoneTask(t, downloadDir, "test.txt")
	srcPath := filepath.FromSlash(taskPath)
	destFile := filepath.Join(destDir, "test.txt")

	scriptPath := getTestScriptPath(t, "move.sh")
	ensureScriptExecutable(t, scriptPath)

	setupScriptTest(t, func(downloader *Downloader) {
		cfg, _ := downloader.GetConfig()
		cfg.Script = &base.ScriptConfig{
			Enable: true,
			Paths:  []string{scriptPath},
		}
		downloader.PutConfig(cfg)

		t.Setenv("GOPEED_TEST_DEST_DIR", destDir)
		downloader.triggerScripts(ScriptEventDownloadDone, task, nil)

		waitForFile(t, destFile, 3*time.Second)
		if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
			t.Errorf("Expected source file to be moved, but it still exists: %s", srcPath)
		}
	})
}

func TestScript_MultipleScripts(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile1 := filepath.Join(tmpDir, "output1.txt")
	outputFile2 := filepath.Join(tmpDir, "output2.txt")
	scriptPath1 := getTestScriptPath(t, "write_output1.sh")
	ensureScriptExecutable(t, scriptPath1)
	scriptPath2 := getTestScriptPath(t, "write_output2.sh")
	ensureScriptExecutable(t, scriptPath2)

	setupScriptTest(t, func(downloader *Downloader) {
		cfg, _ := downloader.GetConfig()
		cfg.Script = &base.ScriptConfig{
			Enable: true,
			Paths:  []string{scriptPath1, scriptPath2},
		}
		downloader.PutConfig(cfg)

		downloadDir := filepath.Join(tmpDir, "downloads")
		task, _ := createDownloadDoneTask(t, downloadDir, "multi.txt")

		t.Setenv("GOPEED_TEST_OUTPUT_FILE_1", outputFile1)
		t.Setenv("GOPEED_TEST_OUTPUT_FILE_2", outputFile2)
		downloader.triggerScripts(ScriptEventDownloadDone, task, nil)

		waitForFile(t, outputFile1, 3*time.Second)
		waitForFile(t, outputFile2, 3*time.Second)
	})
}

func TestScript_EnvironmentVariables(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := getTestScriptPath(t, "env_dump.sh")
	ensureScriptExecutable(t, scriptPath)
	outputFile := filepath.Join(tmpDir, "env_output.txt")

	setupScriptTest(t, func(downloader *Downloader) {
		// executeScripts emits either outcome only after cmd.Run has returned.
		// File existence alone can expose the first of the fixture's five writes.
		finished := make(chan bool, 1)
		downloader.Logger.Logger = downloader.Logger.Hook(zerolog.HookFunc(func(_ *zerolog.Event, _ zerolog.Level, message string) {
			switch message {
			case "script: executed successfully":
				finished <- true
			case "script: failed to execute":
				finished <- false
			}
		}))
		cfg, _ := downloader.GetConfig()
		cfg.Script = &base.ScriptConfig{
			Enable: true,
			Paths:  []string{scriptPath},
		}
		downloader.PutConfig(cfg)

		downloadDir := filepath.Join(tmpDir, "downloads")
		task, taskPath := createDownloadDoneTask(t, downloadDir, "env.txt")

		t.Setenv("GOPEED_TEST_OUTPUT_FILE", outputFile)
		downloader.triggerScripts(ScriptEventDownloadDone, task, nil)

		select {
		case success := <-finished:
			if !success {
				t.Fatal("Environment fixture script exited unsuccessfully; see executor log")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Timeout waiting for environment fixture script to exit")
		}
		content, err := os.ReadFile(outputFile)
		if err != nil {
			t.Fatalf("Failed to read output file: %v", err)
		}

		output := string(content)
		if !strings.Contains(output, "GOPEED_EVENT=DOWNLOAD_DONE") {
			t.Errorf("Expected GOPEED_EVENT in output, got: %s", output)
		}
		if !strings.Contains(output, "GOPEED_TASK_ID="+task.ID) {
			t.Errorf("Expected GOPEED_TASK_ID in output, got: %s", output)
		}
		if !strings.Contains(output, "GOPEED_TASK_NAME="+task.Name()) {
			t.Errorf("Expected GOPEED_TASK_NAME in output, got: %s", output)
		}
		if !strings.Contains(output, "GOPEED_TASK_STATUS="+string(task.Status)) {
			t.Errorf("Expected GOPEED_TASK_STATUS in output, got: %s", output)
		}
		if !strings.Contains(output, "GOPEED_TASK_PATH="+taskPath) {
			t.Errorf("Expected GOPEED_TASK_PATH in output, got: %s", output)
		}
	})
}
