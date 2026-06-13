package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"
)

const (
	// ANSI color codes
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorPurple = "\033[35m"
	colorCyan   = "\033[36m"
	colorWhite  = "\033[37m"
)

func main() {
	var (
		cleanup  = flag.Bool("cleanup", false, "Clean up all test containers and exit")
		verbose  = flag.Bool("v", false, "Verbose output")
		parallel = flag.Int("parallel", 1, "Number of tests to run in parallel")
	)
	flag.Parse()

	runner := &TestRunner{
		verbose:  *verbose,
		parallel: *parallel,
	}

	fmt.Printf("%s🧪 ODDK End-to-End Tests%s\n", colorCyan, colorReset)
	fmt.Printf("═══════════════════════════════\n\n")

	if *cleanup {
		fmt.Printf("%s🧹 Cleaning up all test containers...%s\n", colorBlue, colorReset)
		if err := runner.cleanupAllTestContainers(); err != nil {
			fmt.Printf("%s❌ Cleanup failed: %v%s\n", colorRed, err, colorReset)
			os.Exit(1)
		}
		fmt.Printf("%s✅ Cleanup complete%s\n", colorGreen, colorReset)
		return
	}

	start := time.Now()

	// Register all tests
	allTests := []Test{
		{Name: "FullLifecycle", Fn: testFullLifecycle},
		{Name: "ConsistencyChecks", Fn: testConsistencyChecks},
		{Name: "StartupReconciliation", Fn: testStartupReconciliation},
		{Name: "MultipleInstances", Fn: testMultipleInstances},
		{Name: "CLIDirectUsage", Fn: testCLIDirectUsage},
		{Name: "BackupOperations", Fn: testBackupOperations},
		{Name: "BackupRemovalOperations", Fn: testBackupRemovalOperations},
		{Name: "PasswordOperations", Fn: testPasswordOperations},
		{Name: "DatabaseManagement", Fn: testDatabaseManagement},
		{Name: "CronCRUD", Fn: testCronCRUD},
		{Name: "CronValidation", Fn: testCronValidation},
		{Name: "CronMultipleInstances", Fn: testCronMultipleInstances},
		{Name: "CronCleanupDays", Fn: testCronCleanupDays},
		{
			Name: "CronExecution",
			Fn:   testCronExecution,
			KVMap: map[string]string{
				"cron.debug_ticker_interval.int": "1",
				"cron.debug_force_run.int":       "1",
			},
		},
		{
			Name:      "CronBackupCleanup",
			Fn:        testCronBackupCleanup,
			RunFakeS3: true,
			KVMap: map[string]string{
				"backup.debug_time_machine.int":  "1",
				"cron.debug_ticker_interval.int": "1",
				"cron.debug_force_run.int":       "1",
			},
		},
		{Name: "ParameterGroupOperations", Fn: testParameterGroupOperations},
		{Name: "ParameterGroupValidation", Fn: testParameterGroupValidation},
		{Name: "ParameterGroupInstanceIntegration", Fn: testParameterGroupInstanceIntegration},
		{Name: "DefaultParameterGroupUsage", Fn: testDefaultParameterGroupUsage},
		{Name: "InstanceApply", Fn: testInstanceApply},
		{Name: "NotificationOperations", Fn: NotificationOperations},
		{Name: "CustomKVOperations", Fn: testCustomKVOperations},
		{Name: "CustomKVAPIOperations", Fn: testCustomKVAPIOperations},
		{Name: "OffsiteConfiguration", Fn: testOffsiteConfiguration, RunFakeS3: true},
		{Name: "BackupUploadToS3", Fn: testBackupUploadToS3, RunFakeS3: true},
		{Name: "BackupDownloadFromS3", Fn: testBackupDownloadFromS3, RunFakeS3: true},
		{Name: "BackupRestore", Fn: testBackupRestore},
		{Name: "BackupRestoreLocale", Fn: testBackupRestoreLocale},
		{Name: "PG18Lifecycle", Fn: testPG18Lifecycle},
		{Name: "CustomImageSwitch", Fn: testCustomImageSwitch},
		{Name: "MajorUpgrade", Fn: testMajorUpgrade},
		{Name: "MajorUpgradeCustomImage", Fn: testMajorUpgradeCustomImage},
		{Name: "MajorUpgradeLocale", Fn: testMajorUpgradeLocale},
		// Health tests - require daemon to have been running for a while
		{Name: "HealthEndpoints", Fn: testHealthEndpoints},
		{Name: "HealthDebugFail", Fn: testHealthDebugFail},
		{Name: "HealthNotifications", Fn: testHealthNotifications},
	}

	// Filter tests based on command line arguments
	testNames := flag.Args()
	var tests []Test
	if len(testNames) == 0 {
		// Run all tests if no specific tests specified
		tests = allTests
	} else {
		// Run only specified tests
		testMap := make(map[string]Test)
		for _, test := range allTests {
			testMap[test.Name] = test
		}

		for _, name := range testNames {
			if test, exists := testMap[name]; exists {
				tests = append(tests, test)
			} else {
				fmt.Printf("%s❌ Test '%s' not found. Available tests:%s\n", colorRed, name, colorReset)
				for _, availableTest := range allTests {
					fmt.Printf("  - %s\n", availableTest.Name)
				}
				os.Exit(1)
			}
		}
	}

	if len(tests) == 0 {
		fmt.Printf("%s❌ No tests to run%s\n", colorRed, colorReset)
		os.Exit(1)
	}

	results := runner.runTests(tests)

	duration := time.Since(start)

	fmt.Printf("\n═══════════════════════════════\n")
	fmt.Printf("%s📊 Test Summary%s\n", colorCyan, colorReset)

	passed := 0
	failed := 0
	for _, result := range results {
		if result.Passed {
			passed++
			fmt.Printf("%s✅ %s%s (%.2fs)\n", colorGreen, result.Name, colorReset, result.Duration.Seconds())
		} else {
			failed++
			fmt.Printf("%s❌ %s%s (%.2fs)\n", colorRed, result.Name, colorReset, result.Duration.Seconds())
			if result.Error != "" {
				fmt.Printf("   %s%s%s\n", colorRed, result.Error, colorReset)
			}
		}
	}

	fmt.Printf("\n%s🏁 Total: %d tests, %d passed, %d failed%s (%.2fs)\n",
		colorWhite, len(results), passed, failed, colorReset, duration.Seconds())

	if failed > 0 {
		fmt.Printf("\n%s💡 Tip: Run with --cleanup to clean up test containers%s\n", colorYellow, colorReset)
		os.Exit(1)
	}

	fmt.Printf("\n%s🎉 All tests passed!%s\n", colorGreen, colorReset)
}

type Test struct {
	Name      string
	Fn        func(*TestHarness) error
	KVMap     map[string]string // Key-value pairs to set before starting server
	RunFakeS3 bool              // Whether to run a fake S3 server for this test
}

type TestResult struct {
	Name     string
	Passed   bool
	Duration time.Duration
	Error    string
}

type TestRunner struct {
	verbose  bool
	parallel int
}

func (r *TestRunner) runTests(tests []Test) []TestResult {
	results := make([]TestResult, len(tests))

	for i, test := range tests {
		fmt.Printf("%s🚀 Running: %s%s\n", colorBlue, test.Name, colorReset)

		start := time.Now()
		harness := setupTestHarness(test.Name, test.KVMap, test.RunFakeS3)

		// output message that harness has been setup
		fmt.Printf("%s🏠 Harness setup complete%s\n", colorGreen, colorReset)

		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("test panicked: %v", r)
				}
				harness.cleanup()
			}()

			err = test.Fn(harness)
		}()

		duration := time.Since(start)

		results[i] = TestResult{
			Name:     test.Name,
			Passed:   err == nil,
			Duration: duration,
		}

		if err != nil {
			results[i].Error = err.Error()
			fmt.Printf("%s❌ %s failed: %v%s\n", colorRed, test.Name, err, colorReset)
		} else {
			fmt.Printf("%s✅ %s passed%s (%.2fs)\n", colorGreen, test.Name, colorReset, duration.Seconds())
		}
	}

	return results
}

func (r *TestRunner) cleanupAllTestContainers() error {
	log.Printf("Cleaning up all test containers...")
	return cleanupAllTestContainers()
}
