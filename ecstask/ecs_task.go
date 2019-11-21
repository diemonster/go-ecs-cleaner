package ecstask

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/golang-collections/collections/stack"
	"github.com/jpillora/backoff"
	"github.com/spf13/cobra"
)

// Run is the entrypoint used by the CLI for this set of work.
func Run(cmd *cobra.Command, args []string, flags map[string]interface{}) {
	fmt.Println("running ecs-task")

	// configure AWS connection

	sess, err := session.NewSession(&aws.Config{})
	if err != nil {
		fmt.Println("Error creating AWS session ", err)
		return
	}

	svc := ecs.New(sess)

	// list all clusters

	fmt.Println("collecting clusters...")

	var nextToken *string
	var clusterArns []string

	clusters, nextToken := listClusters(svc, nextToken)
	for _, arn := range clusters {
		clusterArns = append(clusterArns, arn)
	}

	needToResetPrinter := false
	for nextToken != nil {
		clusters, nextToken = listClusters(svc, nextToken)

		for _, arn := range clusters {
			clusterArns = append(clusterArns, arn)
		}

		fmt.Printf("\r(found %d)", len(clusterArns))
		needToResetPrinter = true
	}

	if needToResetPrinter {
		fmt.Println()
		needToResetPrinter = false
	} else {
		fmt.Printf("(found %d)\n", len(clusterArns))
	}

	clusterServiceArns := make(map[string][]string)

	for _, clusterArn := range clusterArns {
		clusterServiceArns[clusterArn] = []string{}
	}

	// list services per cluster

	fmt.Println("collecting services...")

	var numServices int
	nextToken = nil
	for clusterArn := range clusterServiceArns {
		serviceArns, nextToken := listServices(svc, clusterArn, nextToken)
		for _, arn := range serviceArns {
			clusterServiceArns[clusterArn] = append(clusterServiceArns[clusterArn], arn)
			numServices++
		}

		for nextToken != nil {
			serviceArns, nextToken = listServices(svc, clusterArn, nextToken)

			for _, arn := range serviceArns {
				clusterServiceArns[clusterArn] = append(clusterServiceArns[clusterArn], arn)
				numServices++
			}

			fmt.Printf("\r(found %d)", numServices)
			needToResetPrinter = true
		}
	}

	if needToResetPrinter {
		fmt.Println()
		needToResetPrinter = false
	} else {
		fmt.Printf("(found %d)\n", numServices)
	}

	// describe each service

	fmt.Println("describing services...")

	clusterServices := make(map[string][]ecs.Service)
	limit := 10

	for clusterArn, serviceArns := range clusterServiceArns {
		for len(serviceArns) > 0 {
			length := len(serviceArns)
			var (
				serviceArnsChunk []string
				iStart           int
				iEnd             int
			)

			if length >= limit {
				iStart = length - limit
				iEnd = length
			} else {
				iStart = 0
				iEnd = length
			}

			serviceArnsChunk = serviceArns[iStart:iEnd]
			serviceArns = serviceArns[0:iStart]

			services := describeServices(svc, clusterArn, serviceArnsChunk)

			var serviceCollection []ecs.Service

			for _, service := range services {
				serviceCollection = append(serviceCollection, service)
			}

			clusterServices[clusterArn] = serviceCollection
		}
	}

	// gather task definitions

	fmt.Println("collecting task definitions...")

	nextToken = nil
	var allTaskDefinitionArns []string

	taskDefinitionArns, nextToken := listTaskDefinitions(svc, "", "", nextToken)
	for _, arn := range taskDefinitionArns {
		allTaskDefinitionArns = append(allTaskDefinitionArns, arn)
	}

	needToResetPrinter = false
	for nextToken != nil {
		taskDefinitionArns, nextToken = listTaskDefinitions(svc, "", "", nextToken)

		for _, arn := range taskDefinitionArns {
			allTaskDefinitionArns = append(allTaskDefinitionArns, arn)
		}

		fmt.Printf("\r(found %d)", len(allTaskDefinitionArns))
		needToResetPrinter = true
	}

	if needToResetPrinter {
		fmt.Println()
		needToResetPrinter = false
	} else {
		fmt.Printf("(found %d)\n", len(allTaskDefinitionArns))
	}

	// filter out in-use/active task defs

	var inUseTaskDefinitionArns []string

	for _, services := range clusterServices {
		for _, service := range services {
			inUseTaskDefinitionArns = append(inUseTaskDefinitionArns, *service.TaskDefinition)
		}
	}

	fmt.Printf("filtering out %d in-use task definitions\n", len(inUseTaskDefinitionArns))
	if flags["verbose"].(bool) {
		for _, arn := range inUseTaskDefinitionArns {
			fmt.Println(arn)
		}
	}

	allTaskDefinitionArns = removeAFromB(inUseTaskDefinitionArns, allTaskDefinitionArns)

	fmt.Printf("%d task definitions remain\n", len(allTaskDefinitionArns))

	// filter out n most-recent per family

	var inUseTaskDefinitionFamilies []string

	for _, arn := range inUseTaskDefinitionArns {
		r1 := regexp.MustCompile(`([A-Za-z0-9_-]+):([0-9]+)$`)
		r2 := regexp.MustCompile(`^([A-Za-z0-9_-]+):`)
		family := strings.TrimSuffix(r2.FindString(r1.FindString(arn)), ":")
		inUseTaskDefinitionFamilies = append(inUseTaskDefinitionFamilies, family)
	}

	fmt.Println("collecting active-family task definitions...")

	var mostRecentActiveTaskDefinitionArns []string

	nextToken = nil
	for _, family := range inUseTaskDefinitionFamilies {
		nextToken = nil
		needToResetPrinter = false
		var familyTaskDefinitionArns []string

		taskDefinitionArns, nextToken = listTaskDefinitions(svc, family, "DESC", nextToken)
		for _, arn := range taskDefinitionArns {
			familyTaskDefinitionArns = append(familyTaskDefinitionArns, arn)
		}

		for nextToken != nil {
			taskDefinitionArns, nextToken = listTaskDefinitions(svc, family, "DESC", nextToken)

			for _, arn := range taskDefinitionArns {
				familyTaskDefinitionArns = append(familyTaskDefinitionArns, arn)
			}

			if flags["verbose"].(bool) {
				fmt.Printf("\r(found %d)", len(familyTaskDefinitionArns))
				needToResetPrinter = true
			}
		}

		if flags["verbose"].(bool) {
			if needToResetPrinter {
				fmt.Println()
				needToResetPrinter = false
			} else {
				fmt.Printf("(found %d)\n", len(familyTaskDefinitionArns))
			}
		}

		familyTaskDefinitionArns = removeAFromB(inUseTaskDefinitionArns, familyTaskDefinitionArns)

		mostRecentCutoff := flags["cutoff"].(int)
		if len(familyTaskDefinitionArns) > mostRecentCutoff {
			familyTaskDefinitionArns = familyTaskDefinitionArns[0:mostRecentCutoff]
		}

		for _, arn := range familyTaskDefinitionArns {
			mostRecentActiveTaskDefinitionArns = append(mostRecentActiveTaskDefinitionArns, arn)
		}

	}

	fmt.Printf("filtering out %d recent task definitions across %d families\n", len(mostRecentActiveTaskDefinitionArns), len(inUseTaskDefinitionFamilies))
	if flags["verbose"].(bool) {
		for _, arn := range mostRecentActiveTaskDefinitionArns {
			fmt.Println(arn)
		}
	}

	allTaskDefinitionArns = removeAFromB(mostRecentActiveTaskDefinitionArns, allTaskDefinitionArns)

	fmt.Printf("%d task definitions to deregister\n", len(allTaskDefinitionArns))

	// what's left will be removed (unless dry-run)

	if len(allTaskDefinitionArns) > 0 {
		if flags["apply"].(bool) {
			fmt.Printf("`--apply` flag present, deregistering %d task definitions...\n", len(allTaskDefinitionArns))

			deregisterTaskDefinitions(svc, allTaskDefinitionArns, flags["parallel"].(int), flags["verbose"].(bool), flags["debug"].(bool))

		} else {
			fmt.Println("\nthis is a dry run")
			fmt.Println("use the `--apply` flag to deregister these task definitions")
		}
	}

	fmt.Println("\nProcess finished.")
}

func listClusters(svc EcsSvc, nextToken *string) ([]string, *string) {
	listClustersInput := &ecs.ListClustersInput{
		NextToken: nextToken,
	}

	listClustersOutput, err := svc.ListClusters(listClustersInput)
	if err != nil {
		fmt.Println("Error listing clusters ", err)
		return []string{}, nil
	}

	var clusterArns []string
	for _, arn := range listClustersOutput.ClusterArns {
		clusterArns = append(clusterArns, *arn)
	}

	nextToken = listClustersOutput.NextToken
	return clusterArns, nextToken
}

func listServices(svc EcsSvc, clusterArn string, nextToken *string) ([]string, *string) {
	listServicesInput := &ecs.ListServicesInput{
		Cluster:   aws.String(clusterArn),
		NextToken: nextToken,
	}

	listServicesOutput, err := svc.ListServices(listServicesInput)
	if err != nil {
		fmt.Println("Error listing services, ", err)
		return []string{}, nil
	}

	var serviceArns []string
	for _, arn := range listServicesOutput.ServiceArns {
		serviceArns = append(serviceArns, *arn)
	}

	nextToken = listServicesOutput.NextToken
	return serviceArns, nextToken
}

func describeServices(svc EcsSvc, clusterArn string, serviceArns []string) []ecs.Service {
	var inputServices []*string

	for _, serviceArn := range serviceArns {
		inputServices = append(inputServices, aws.String(serviceArn))
	}

	describeServicesInput := &ecs.DescribeServicesInput{
		Cluster:  aws.String(clusterArn),
		Services: inputServices,
	}

	ecsServices, err := svc.DescribeServices(describeServicesInput)
	if err != nil {
		fmt.Println("Error describing services, ", err)
		return []ecs.Service{}
	}

	var services []ecs.Service

	for _, ecsService := range ecsServices.Services {
		services = append(services, *ecsService)
	}

	return services
}

func listTaskDefinitions(svc EcsSvc, familyPrefix, sort string, nextToken *string) ([]string, *string) {
	listTaskDefinitionsInput := &ecs.ListTaskDefinitionsInput{
		NextToken: nextToken,
	}

	if familyPrefix != "" {
		listTaskDefinitionsInput.SetFamilyPrefix(familyPrefix)
	}

	if sort != "" {
		listTaskDefinitionsInput.SetSort(sort)
	}

	listTaskDefinitionsOutput, err := svc.ListTaskDefinitions(listTaskDefinitionsInput)
	if err != nil {
		fmt.Println("Error listing task definitions,", err)
		return []string{}, nil
	}

	var taskDefinitionArns []string
	for _, arn := range listTaskDefinitionsOutput.TaskDefinitionArns {
		taskDefinitionArns = append(taskDefinitionArns, *arn)
	}

	nextToken = listTaskDefinitionsOutput.NextToken
	return taskDefinitionArns, nextToken
}

func removeAFromB(a, b []string) []string {
	var diff []string
	m := make(map[string]int)

	for _, item := range b {
		m[item] = 1
	}

	for _, item := range a {
		if m[item] != 0 {
			m[item]++
		}
	}

	for k, v := range m {
		if v == 1 {
			diff = append(diff, k)
		}
	}

	return diff
}

// Job carries information through a Job channel.
type Job struct {
	Arn string
}

// Result carries information through a Result channel.
type Result struct {
	Arn string
	Err error
}

func deregisterTaskDefinitions(svc EcsSvc, taskDefinitionArns []string, parallel int, verbose, debug bool) {
	if len(taskDefinitionArns) < parallel {
		parallel = len(taskDefinitionArns)
	}

	jobsChan := make(chan Job, parallel) // closed by dispatcher
	resultsChan := make(chan Result, parallel)
	quitChan := make(chan bool, parallel)

	defer close(resultsChan)
	defer close(quitChan)

	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go worker(svc, &wg, jobsChan, resultsChan, quitChan)
	}

	wg.Add(1)
	go dispatcher(&wg, taskDefinitionArns, parallel, jobsChan, resultsChan, quitChan, verbose, debug)

	wg.Wait()
}

func worker(svc EcsSvc, wg *sync.WaitGroup, jobsChan <-chan Job, resultsChan chan<- Result, quitChan <-chan bool) {
	defer wg.Done()

	for job := range jobsChan {
		select {
		case <-quitChan:
			return
		default:
			input := &ecs.DeregisterTaskDefinitionInput{
				TaskDefinition: aws.String(job.Arn),
			}

			_, err := svc.DeregisterTaskDefinition(input)

			result := Result{Arn: job.Arn, Err: err}
			resultsChan <- result
		}
	}
}

func dispatcher(wg *sync.WaitGroup, arns []string, parallel int, jobsChan chan Job, resultsChan chan Result, quitChan chan<- bool, verbose, debug bool) {
	defer wg.Done()
	defer close(jobsChan)

	jobs := stack.New()
	for _, arn := range arns {
		jobs.Push(Job{arn})
	}

	var failedJobs []Result
	var numCompletedJobs int
	numJobsToComplete := len(arns)

	preload := 1
	if parallel > 1 {
		preload = parallel - 1
	}

	for i := 0; i < preload; i++ {
		jobsChan <- jobs.Pop().(Job)
	}

	b := &backoff.Backoff{
		Min:    100 * time.Millisecond,
		Max:    2 * time.Minute,
		Jitter: true,
	}

	for numCompletedJobs < numJobsToComplete {
		result := <-resultsChan
		if result.Err != nil {
			if isThrottlingError(result.Err) {
				t := b.Duration()
				if debug {
					fmt.Printf("\nbackoff triggered for %s,", result.Arn)
					fmt.Printf("\nwaiting for %v\n", t)
				}

				time.Sleep(t)
				jobs.Push(Job{Arn: result.Arn})

			} else if isStopworthyError(result.Err) {
				fmt.Printf("\nEncountered stopworthy error %v\nStopping run.\n", result.Err)
				for i := 0; i < parallel; i++ {
					quitChan <- true
				}

				return

			} else {
				failedJobs = append(failedJobs, result)
				numJobsToComplete--
			}

		} else {
			b.Reset()
			numCompletedJobs++
		}

		fmt.Printf("\r%d deregistered task definitions, %d errored", numCompletedJobs, len(failedJobs))

		if jobs.Len() > 0 {
			jobsChan <- jobs.Pop().(Job)
		}
	}

	if len(failedJobs) > 0 {
		fmt.Printf("\n%d task definitions errored.\n", len(failedJobs))
		if verbose {
			for _, result := range failedJobs {
				fmt.Printf("%s: %v\n", result.Arn, result.Err)
			}
		}
	}
}

func isThrottlingError(err error) bool {
	if awsErr, ok := err.(awserr.Error); ok {
		code := awsErr.Code()

		if code == "Throttling" || code == "ThrottlingException" {
			return true
		}

		message := strings.ToLower(awsErr.Message())
		if code == "ClientException" && strings.Contains(message, "too many concurrent attempts") {
			return true
		}
	}

	return false
}

func isStopworthyError(err error) bool {
	if awsErr, ok := err.(awserr.Error); ok {
		code := awsErr.Code()

		if code == "ExpiredTokenException" {
			return true
		}
	}

	return false
}
