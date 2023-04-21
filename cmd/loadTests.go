package cmd

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codeready-toolchain/toolchain-e2e/setup/auth"
	"github.com/codeready-toolchain/toolchain-e2e/setup/configuration"
	"github.com/codeready-toolchain/toolchain-e2e/setup/metrics"
	"github.com/codeready-toolchain/toolchain-e2e/setup/metrics/queries"
	"github.com/codeready-toolchain/toolchain-e2e/setup/terminal"
	"github.com/codeready-toolchain/toolchain-e2e/setup/users"
	"github.com/codeready-toolchain/toolchain-e2e/setup/wait"
	"github.com/google/uuid"
	"github.com/gosuri/uiprogress"
	"github.com/gosuri/uitable/util/strutil"
	"github.com/redhat-appstudio/e2e-tests/pkg/constants"
	"github.com/redhat-appstudio/e2e-tests/pkg/framework"
	"github.com/redhat-appstudio/e2e-tests/pkg/utils"
	"github.com/spf13/cobra"
	k8swait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"knative.dev/pkg/apis"
)

var (
	usernamePrefix       = "testuser"
	numberOfUsers        int
	userBatches          int
	waitPipelines        bool
	verbose              bool
	QuarkusDevfileSource string = "https://github.com/devfile-samples/devfile-sample-code-with-quarkus"
	token                string
	logConsole           bool
	failFast             bool
	disableMetrics       bool
	threadCount          int
)

var (
	AverageUserCreationTime            []time.Duration
	AverageResourceCreationTimePerUser []time.Duration
	AveragePipelineRunTimePerUser      []time.Duration
	FailedUserCreations                []int64
	FailedResourceCreations            []int64
	FailedPipelineRuns                 []int64
	errorOccurredMap                   map[int]ErrorOccurrence
	errorMutex                         = &sync.Mutex{}
	threadsWG                          sync.WaitGroup
)

type ErrorOccurrence struct {
	ErrorNumber   int    `json:"errorNumber"`
	LatestMessage string `json:"latestMessage"`
	Count         int    `json:"count"`
}

type LogData struct {
	Timestamp                         string            `json:"timestamp"`
	EndTimestamp                      string            `json:"endTimestamp"`
	MachineName                       string            `json:"machineName"`
	BinaryDetails                     string            `json:"binaryDetails"`
	NumberOfThreads                   int               `json:"threads"`
	NumberOfUsersPerThread            int               `json:"usersPerThread"`
	BatchSize                         int               `json:"threadBatchSize"`
	NumberOfUsers                     int               `json:"totalUsers"`
	LoadTestCompletionStatus          string            `json:"status"`
	AverageTimeToSpinUpUsers          float64           `json:"createUserTimeAvg"`
	AverageTimeToCreateResources      float64           `json:"createResourcesTimeAvg"`
	AverageTimeToRunPipelines         float64           `json:"runPipelineTimeAvg"`
	UserCreationFailureCount          int64             `json:"createUserFailures"`
	UserCreationFailurePercentage     float64           `json:"createUserFailureRate"`
	ResourceCreationFailureCount      int64             `json:"createResourcesFailures"`
	ResourceCreationFailurePercentage float64           `json:"createResourcesFailureRate"`
	PipelineRunFailureCount           int64             `json:"runPipelineFailures"`
	PipelineRunFailurePercentage      float64           `json:"runPipelineFailureRate"`
	ErrorsOccurred                    []ErrorOccurrence `json:"errors"`
}

func createLogDataJSON(outputFile string, logDataInput LogData) error {
	jsonData, err := json.MarshalIndent(logDataInput, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshalling JSON: %v", err)
	}

	err = os.WriteFile(outputFile, jsonData, 0644) // Replace ioutil.WriteFile with os.WriteFile
	if err != nil {
		return fmt.Errorf("error writing JSON file: %v", err)
	}

	return nil
}

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "load-test",
	Short: "Used to Generate Users and Run Load Tests on AppStudio.",
	Long:  `Used to Generate Users and Run Load Tests on AppStudio.`,
	// Uncomment the following line if your bare application
	// has an action associated with it:
	// Run: func(cmd *cobra.Command, args []string) { },
	SilenceErrors: true,
	SilenceUsage:  false,
	Args:          cobra.NoArgs,
	Run:           setup,
}

// ExecuteLoadTest adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func ExecuteLoadTest() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	// Here you will define your flags and configuration settings.
	// Cobra supports persistent flags, which, if defined here,
	// will be global for your application.
	// Cobra also supports local flags, which will only run
	// when this action is called directly.
	rootCmd.Flags().StringVar(&usernamePrefix, "username", usernamePrefix, "the prefix used for usersignup names")
	// TODO use a custom kubeconfig and introduce debug logging and trace
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "if 'debug' traces should be displayed in the console")
	rootCmd.Flags().IntVarP(&numberOfUsers, "users", "u", 5, "the number of user accounts to provision")
	rootCmd.Flags().IntVarP(&userBatches, "batch", "b", 5, "create user accounts in batches of N, increasing batch size may cause performance problems")
	rootCmd.Flags().BoolVarP(&waitPipelines, "waitpipelines", "w", false, "if you want to wait for pipelines to finish")
	rootCmd.Flags().BoolVarP(&logConsole, "log-to-console", "l", false, "if you want to log to console in addition to the log file")
	rootCmd.Flags().BoolVar(&failFast, "fail-fast", false, "if you want the test to fail fast at first failure")
	rootCmd.Flags().BoolVar(&disableMetrics, "disable-metrics", false, "if you want to disable metrics gathering")
	rootCmd.Flags().IntVarP(&threadCount, "threads", "t", 1, "number of concurrent threads to execute")
}

func logError(errCode int, message string) {
	msg := fmt.Sprintf("Error #%d: %s", errCode, message)
	if failFast {
		klog.Fatalln(msg)
	} else {
		klog.Errorln(msg)
	}
	errorMutex.Lock()
	defer errorMutex.Unlock()
	errorOccurrence, ok := errorOccurredMap[errCode]
	if ok {
		errorOccurrence.Count += 1
		errorOccurrence.LatestMessage = message
		errorOccurredMap[errCode] = errorOccurrence
	} else {
		errorOccurrence := ErrorOccurrence{
			ErrorNumber:   errCode,
			LatestMessage: message,
			Count:         1,
		}
		errorOccurredMap[errCode] = errorOccurrence
	}
}

func setKlogFlag(fs flag.FlagSet, name string, value string) {
	err := fs.Set(name, value)
	if err != nil {
		klog.Fatalf("Unable to set klog flag %s: %v", name, err)
	}
}

func setup(cmd *cobra.Command, args []string) {
	cmd.SilenceUsage = true
	term := terminal.New(cmd.InOrStdin, cmd.OutOrStdout, verbose)

	/*
		used for the json output file -
		loadTestsTimestamp - loadTests start timestamp, used for the json output file
	*/
	// loadTestsTimestamp := time.Now().Format("2006/01/02 15:04:05")
	loadTestsTimestamp := time.Now().Format("2006-01-02T15:04:05Z07:00")

	logFile, err := os.Create("load-tests.log")
	if err != nil {
		klog.Fatalf("Error creating log file: %v", err)
	}
	var fs flag.FlagSet
	klog.InitFlags(&fs)
	setKlogFlag(fs, "log_file", logFile.Name())
	setKlogFlag(fs, "logtostderr", "false")
	setKlogFlag(fs, "alsologtostderr", strconv.FormatBool(logConsole))

	if numberOfUsers%userBatches != 0 {
		klog.Fatalf("Please Provide Correct Batches!")
		os.Exit(1)
	}

	klog.Infof("Number of threads: %d", threadCount)
	klog.Infof("Number of users per thread: %d", numberOfUsers)
	klog.Infof("Batch Size per thread: %d", userBatches)

	klog.Infof("🕖 initializing...\n")
	framework, err := framework.NewFramework("load-tests")
	if err != nil {
		klog.Fatalf("error creating client-go %v", err)
	}

	if len(token) == 0 {
		token, err = auth.GetTokenFromOC()
		if err != nil {
			tokenRequestURI, err := auth.GetTokenRequestURI(framework.AsKubeAdmin.CommonController.KubeRest()) // authorization.FindTokenRequestURI(framework.CommonController.KubeRest())
			if err != nil {
				klog.Fatalf("a token is required to capture metrics, use oc login to log into the cluster: %v", err)
			}
			klog.Fatalf("a token is required to capture metrics, use oc login to log into the cluster. alternatively request a token and use the token flag: %v", tokenRequestURI)
		}
	}

	var stopMetrics chan struct{}
	var metricsInstance *metrics.Gatherer
	if !disableMetrics {
		metricsInstance = metrics.NewEmpty(term, framework.AsKubeAdmin.CommonController.KubeRest(), 10*time.Minute)

		prometheusClient := metrics.GetPrometheusClient(term, framework.AsKubeAdmin.CommonController.KubeRest(), token)

		metricsInstance.AddQueries(
			queries.QueryClusterCPUUtilisation(prometheusClient),
			queries.QueryClusterMemoryUtilisation(prometheusClient),
			queries.QueryNodeMemoryUtilisation(prometheusClient),
			queries.QueryEtcdMemoryUsage(prometheusClient),
			queries.QueryWorkloadCPUUsage(prometheusClient, constants.OLMOperatorNamespace, constants.OLMOperatorWorkload),
			queries.QueryWorkloadMemoryUsage(prometheusClient, constants.OLMOperatorNamespace, constants.OLMOperatorWorkload),
			queries.QueryOpenshiftKubeAPIMemoryUtilisation(prometheusClient),
			queries.QueryWorkloadCPUUsage(prometheusClient, constants.OSAPIServerNamespace, constants.OSAPIServerWorkload),
			queries.QueryWorkloadMemoryUsage(prometheusClient, constants.OSAPIServerNamespace, constants.OSAPIServerWorkload),
			queries.QueryWorkloadCPUUsage(prometheusClient, constants.HostOperatorNamespace, constants.HostOperatorWorkload),
			queries.QueryWorkloadMemoryUsage(prometheusClient, constants.HostOperatorNamespace, constants.HostOperatorWorkload),
			queries.QueryWorkloadCPUUsage(prometheusClient, constants.MemberOperatorNamespace, constants.MemberOperatorWorkload),
			queries.QueryWorkloadMemoryUsage(prometheusClient, constants.MemberOperatorNamespace, constants.MemberOperatorWorkload),
			queries.QueryWorkloadCPUUsage(prometheusClient, "application-service", "application-service-application-service-controller-manager"),
			queries.QueryWorkloadMemoryUsage(prometheusClient, "application-service", "application-service-application-service-controller-manager"),
			queries.QueryWorkloadCPUUsage(prometheusClient, "build-service", "build-service-controller-manager"),
			queries.QueryWorkloadMemoryUsage(prometheusClient, "build-service", "build-service-controller-manager"),
		)
		stopMetrics = metricsInstance.StartGathering()

		klog.Infof("Sleeping till all metrics queries gets init")
		time.Sleep(time.Second * 10)
	}

	klog.Infof("🍿 provisioning users...\n")

	overallCount := numberOfUsers * threadCount

	uip := uiprogress.New()
	uip.Start()

	AppStudioUsersBar := uip.AddBar(overallCount).AppendCompleted().PrependFunc(func(b *uiprogress.Bar) string {
		return strutil.PadLeft(fmt.Sprintf("Creating AppStudio Users (%d/%d) [%d failed]", b.Current(), overallCount, sumFromArray(FailedUserCreations)), userBatches, ' ')
	})

	ResourcesBar := uip.AddBar(overallCount).AppendCompleted().PrependFunc(func(b *uiprogress.Bar) string {
		return strutil.PadLeft(fmt.Sprintf("Creating AppStudio User Resources (%d/%d) [%d failed]", b.Current(), overallCount, sumFromArray(FailedResourceCreations)), userBatches, ' ')
	})

	PipelinesBar := uip.AddBar(overallCount).AppendCompleted().PrependFunc(func(b *uiprogress.Bar) string {
		return strutil.PadLeft(fmt.Sprintf("Waiting for pipelines to finish (%d/%d) [%d failed]", b.Current(), overallCount, sumFromArray(FailedPipelineRuns)), userBatches, ' ')
	})

	AverageUserCreationTime = make([]time.Duration, threadCount)
	AverageResourceCreationTimePerUser = make([]time.Duration, threadCount)
	AveragePipelineRunTimePerUser = make([]time.Duration, threadCount)
	FailedUserCreations = make([]int64, threadCount)
	FailedResourceCreations = make([]int64, threadCount)
	FailedPipelineRuns = make([]int64, threadCount)
	errorOccurredMap = make(map[int]ErrorOccurrence)

	threadsWG.Add(threadCount)
	for thread := 0; thread < threadCount; thread++ {
		go userJourneyThread(framework, thread, AppStudioUsersBar, ResourcesBar, PipelinesBar)
	}

	// Todo add cleanup functions that will delete user signups

	threadsWG.Wait()
	uip.Stop()

	loadTestsEndTimestamp := time.Now().Format("2006-01-02T15:04:05Z07:00")
	averageTimeToSpinUpUsers := averageDurationFromArray(AverageUserCreationTime, overallCount)
	averageTimeToCreateResources := averageDurationFromArray(AverageResourceCreationTimePerUser, overallCount)
	averageTimeToRunPipelines := averageDurationFromArray(AveragePipelineRunTimePerUser, overallCount)
	userCreationFailureCount := sumFromArray(FailedUserCreations)
	userCreationFailurePercentage := 100 * float64(sumFromArray(FailedUserCreations)) / float64(overallCount)
	resourceCreationFailureCount := sumFromArray(FailedResourceCreations)
	resourceCreationFailurePercentage := 100 * float64(sumFromArray(FailedResourceCreations)) / float64(overallCount)
	pipelineRunFailureCount := sumFromArray(FailedPipelineRuns)
	PipelineRunFailurePercentage := 100 * float64(sumFromArray(FailedPipelineRuns)) / float64(overallCount)

	klog.Infof("🏁 Load Test Completed!")
	klog.Infof("📈 Results 📉")
	klog.Infof("Average Time taken to spin up users: %.2f s", averageTimeToSpinUpUsers)
	klog.Infof("Average Time taken to Create Resources: %.2f s", averageTimeToCreateResources)
	klog.Infof("Average Time taken to Run Pipelines: %.2f s", averageTimeToRunPipelines)
	klog.Infof("Number of times user creation failed: %d (%.2f %%)", userCreationFailureCount, userCreationFailurePercentage)
	klog.Infof("Number of times resource creation failed: %d (%.2f %%)", resourceCreationFailureCount, resourceCreationFailurePercentage)
	klog.Infof("Number of times pipeline run failed: %d (%.2f %%)", pipelineRunFailureCount, PipelineRunFailurePercentage)
	for errorCode, errorOccurrence := range errorOccurredMap {
		klog.Infof("Number of error #%d occured: %d", errorCode, errorOccurrence.Count)
	}

	klog.StopFlushDaemon()
	klog.Flush()
	if !disableMetrics {
		defer close(stopMetrics)
		metricsInstance.PrintResults()
	}

	/*
		fetch the below fields:
		machineName string - the machine on-which the loadTests are run,
		binaryDetails string - binary details of the program that runs the tests
	*/
	machineName, err := os.Hostname()
	if err != nil {
		klog.Errorf("error getting hostname: %v\n", err)
		return
	}

	goVersion := runtime.Version()
	goOS := runtime.GOOS
	goArch := runtime.GOARCH
	binaryDetails := fmt.Sprintf("Built with %s for %s/%s", goVersion, goOS, goArch)

	var errorOccurredList []ErrorOccurrence
	for _, errorOccurrance := range errorOccurredMap {
		errorOccurredList = append(errorOccurredList, errorOccurrance)
	}

	logDataInput := LogData{
		Timestamp:                         loadTestsTimestamp,
		EndTimestamp:                      loadTestsEndTimestamp,
		MachineName:                       machineName,
		BinaryDetails:                     binaryDetails,
		NumberOfThreads:                   threadCount,
		NumberOfUsersPerThread:            numberOfUsers,
		NumberOfUsers:                     overallCount,
		BatchSize:                         userBatches,
		LoadTestCompletionStatus:          "Completed",
		AverageTimeToSpinUpUsers:          averageTimeToSpinUpUsers,
		AverageTimeToCreateResources:      averageTimeToCreateResources,
		AverageTimeToRunPipelines:         averageTimeToRunPipelines,
		UserCreationFailureCount:          userCreationFailureCount,
		UserCreationFailurePercentage:     userCreationFailurePercentage,
		ResourceCreationFailureCount:      resourceCreationFailureCount,
		ResourceCreationFailurePercentage: resourceCreationFailurePercentage,
		PipelineRunFailureCount:           pipelineRunFailureCount,
		PipelineRunFailurePercentage:      PipelineRunFailurePercentage,
		ErrorsOccurred:                    errorOccurredList,
	}

	err = createLogDataJSON("load-tests.json", logDataInput)
	if err != nil {
		klog.Errorf("error while marshalling JSON: %v\n", err)
	}
}

func averageDurationFromArray(duration []time.Duration, count int) float64 {
	avg := 0
	for _, i := range duration {
		avg += int(i.Seconds())
	}
	return float64(avg) / float64(count)
}

func sumFromArray(array []int64) int64 {
	sum := int64(0)
	for _, i := range array {
		sum += i
	}
	return sum
}

func userJourneyThread(framework *framework.Framework, threadIndex int, usersBar *uiprogress.Bar, resourcesBar *uiprogress.Bar, pipelinesBar *uiprogress.Bar) {
	chUsers := make(chan int, numberOfUsers)
	chPipelines := make(chan int, numberOfUsers)

	var wg sync.WaitGroup

	if waitPipelines {
		wg.Add(3)
	} else {
		wg.Add(2)
	}

	go func() {
	UserLoop:
		for userIndex := 1; userIndex <= numberOfUsers; userIndex++ {
			startTime := time.Now()
			username := fmt.Sprintf("%s-%04d", usernamePrefix, threadIndex*numberOfUsers+userIndex)
			if err := users.Create(framework.AsKubeAdmin.CommonController.KubeRest(), username, constants.HostOperatorNamespace, constants.MemberOperatorNamespace); err != nil {
				logError(1, fmt.Sprintf("Unable to provision user '%s': %v", username, err))
				atomic.StoreInt64(&FailedUserCreations[threadIndex], atomic.AddInt64(&FailedUserCreations[threadIndex], 1))
				continue
			}
			if userIndex%userBatches == 0 {
				for i := userIndex - userBatches + 1; i <= userIndex; i++ {
					usernamespace := fmt.Sprintf("%s-%04d-tenant", usernamePrefix, threadIndex*numberOfUsers+userIndex)
					if err := wait.ForNamespace(framework.AsKubeAdmin.CommonController.KubeRest(), usernamespace); err != nil {
						logError(2, fmt.Sprintf("Unable to find namespace '%s' within %v: %v", usernamespace, configuration.DefaultTimeout, err))
						atomic.StoreInt64(&FailedUserCreations[threadIndex], atomic.AddInt64(&FailedUserCreations[threadIndex], 1))
						continue UserLoop
					}
					chUsers <- i
				}
			}
			UserCreationTime := time.Since(startTime)
			AverageUserCreationTime[threadIndex] += UserCreationTime
			usersBar.Incr()
		}
		close(chUsers)
		wg.Done()
	}()

	go func() {
		for userIndex := range chUsers {
			startTime := time.Now()
			username := fmt.Sprintf("%s-%04d", usernamePrefix, threadIndex*numberOfUsers+userIndex)
			usernamespace := fmt.Sprintf("%s-tenant", username)
			_, errors := framework.AsKubeAdmin.CommonController.CreateRegistryAuthSecret(
				constants.RegistryAuthSecretName,
				usernamespace,
				utils.GetDockerConfigJson(),
			)
			if errors != nil {
				logError(3, fmt.Sprintf("Unable to create the secret %s in namespace %s: %v", constants.RegistryAuthSecretName, usernamespace, errors))
				atomic.StoreInt64(&FailedResourceCreations[threadIndex], atomic.AddInt64(&FailedResourceCreations[threadIndex], 1))
				continue
			}
			// time.Sleep(time.Second * 2)
			ApplicationName := fmt.Sprintf("%s-app", username)
			app, err := framework.AsKubeAdmin.HasController.CreateHasApplication(ApplicationName, usernamespace)
			if err != nil {
				logError(4, fmt.Sprintf("Unable to create the Application %s: %v", ApplicationName, err))
				atomic.StoreInt64(&FailedResourceCreations[threadIndex], atomic.AddInt64(&FailedResourceCreations[threadIndex], 1))
				continue
			}
			gitopsRepoTimeout := 60 * time.Second
			if err := utils.WaitUntil(framework.AsKubeAdmin.HasController.ApplicationGitopsRepoExists(app.Status.Devfile), gitopsRepoTimeout); err != nil {
				logError(5, fmt.Sprintf("Unable to create application %s gitops repo within %v: %v", ApplicationName, gitopsRepoTimeout, err))
				atomic.StoreInt64(&FailedResourceCreations[threadIndex], atomic.AddInt64(&FailedResourceCreations[threadIndex], 1))
				continue
			}
			ComponentName := fmt.Sprintf("%s-component", username)
			ComponentContainerImage := fmt.Sprintf("quay.io/%s/test-images:%s-%s", utils.GetQuayIOOrganization(), username, strings.Replace(uuid.New().String(), "-", "", -1))
			component, err := framework.AsKubeAdmin.HasController.CreateComponent(
				ApplicationName,
				ComponentName,
				usernamespace,
				QuarkusDevfileSource,
				"",
				"",
				ComponentContainerImage,
				"",
				true,
			)
			if err != nil {
				logError(6, fmt.Sprintf("Unable to create the Component %s: %v", ComponentName, err))
				atomic.StoreInt64(&FailedResourceCreations[threadIndex], atomic.AddInt64(&FailedResourceCreations[threadIndex], 1))
				continue
			}
			if component.Name != ComponentName {
				logError(7, fmt.Sprintf("Actual component name (%s) does not match expected (%s): %v", component.Name, ComponentName, err))
				atomic.StoreInt64(&FailedResourceCreations[threadIndex], atomic.AddInt64(&FailedResourceCreations[threadIndex], 1))
				continue
			}
			if userIndex%userBatches == 0 {
				for i := userIndex - userBatches + 1; i <= userIndex; i++ {
					time.Sleep(time.Second * 1)
					// Todo Add validation after each batch
				}
			}
			ResourceCreationTime := time.Since(startTime)
			AverageResourceCreationTimePerUser[threadIndex] += ResourceCreationTime
			chPipelines <- userIndex
			resourcesBar.Incr()
		}
		close(chPipelines)
		wg.Done()
	}()

	if waitPipelines {
		go func() {
			for userIndex := range chPipelines {
				username := fmt.Sprintf("%s-%04d", usernamePrefix, threadIndex*numberOfUsers+userIndex)
				usernamespace := fmt.Sprintf("%s-tenant", username)
				ComponentName := fmt.Sprintf("%s-component", username)
				ApplicationName := fmt.Sprintf("%s-app", username)
				DefaultRetryInterval := time.Millisecond * 200
				DefaultTimeout := time.Minute * 60
				error := k8swait.Poll(DefaultRetryInterval, DefaultTimeout, func() (done bool, err error) {
					pipelineRun, err := framework.AsKubeAdmin.HasController.GetComponentPipelineRun(ComponentName, ApplicationName, usernamespace, "")
					if err != nil {
						return false, nil
					}
					if pipelineRun.IsDone() {
						AveragePipelineRunTimePerUser[threadIndex] += pipelineRun.Status.CompletionTime.Sub(pipelineRun.CreationTimestamp.Time)
						succeededCondition := pipelineRun.Status.GetCondition(apis.ConditionSucceeded)
						if succeededCondition.IsFalse() {
							logError(8, fmt.Sprintf("Pipeline run for %s/%s failed due to %v: %v", ApplicationName, ComponentName, succeededCondition.Reason, succeededCondition.Message))
							atomic.StoreInt64(&FailedPipelineRuns[threadIndex], atomic.AddInt64(&FailedPipelineRuns[threadIndex], 1))
						}
						pipelinesBar.Incr()
					}
					return pipelineRun.IsDone(), nil
				})
				if error != nil {
					logError(9, fmt.Sprintf("Pipeline run for %s/%s failed to succeed within %v: %v", ApplicationName, ComponentName, DefaultTimeout, error))
					atomic.StoreInt64(&FailedPipelineRuns[threadIndex], atomic.AddInt64(&FailedPipelineRuns[threadIndex], 1))
					continue
				}
			}
			wg.Done()
		}()
	}
	wg.Wait()
	threadsWG.Done()
}
