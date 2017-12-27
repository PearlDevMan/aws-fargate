package cmd

import (
	"fmt"
	"regexp"
	"strings"

	CWL "github.com/jpignata/fargate/cloudwatchlogs"
	"github.com/jpignata/fargate/console"
	"github.com/jpignata/fargate/docker"
	EC2 "github.com/jpignata/fargate/ec2"
	ECR "github.com/jpignata/fargate/ecr"
	ECS "github.com/jpignata/fargate/ecs"
	ELBV2 "github.com/jpignata/fargate/elbv2"
	"github.com/jpignata/fargate/git"
	IAM "github.com/jpignata/fargate/iam"
	"github.com/spf13/cobra"
)

const validRuleTypesPattern = "(?i)^host|path$"

type ServiceCreateOperation struct {
	ServiceName      string
	Cpu              string
	Image            string
	Memory           string
	Port             Port
	LoadBalancerArn  string
	LoadBalancerName string
	Rules            []ELBV2.Rule
	Elbv2            ELBV2.ELBV2
	EnvVars          []ECS.EnvVar
}

func (o *ServiceCreateOperation) SetPort(inputPort string) {
	var msgs []string

	port := inflatePort(inputPort)
	validProtocols := regexp.MustCompile(validProtocolsPattern)

	if !validProtocols.MatchString(port.Protocol) {
		msgs = append(msgs, fmt.Sprintf("Invalid protocol %s [specify TCP, HTTP, or HTTPS]", port.Protocol))
	}

	if port.Port < 1 || port.Port > 65535 {
		msgs = append(msgs, fmt.Sprintf("Invalid port %d [specify within 1 - 65535]", port.Port))
	}

	if len(msgs) > 0 {
		console.ErrorExit(fmt.Errorf(strings.Join(msgs, ", ")), "Invalid command line flags")
	}

	o.Port = port
}

func (o *ServiceCreateOperation) Validate() {
	err := validateCpuAndMemory(o.Cpu, o.Memory)

	if err != nil {
		console.ErrorExit(err, "Invalid settings: %d CPU units / %d MiB", o.Cpu, o.Memory)
	}
}

func (o *ServiceCreateOperation) SetLoadBalancer(lb string) {
	loadBalancer := o.Elbv2.DescribeLoadBalancer(lb)

	if loadBalancer.Type == "network" {
		if o.Port.Protocol != "TCP" {
			console.ErrorExit(fmt.Errorf("network load balancer %s only supports TCP", lb), "Invalid load balancer and protocol")
		}
	}

	if loadBalancer.Type == "application" {
		if !(o.Port.Protocol == "HTTP" || o.Port.Protocol == "HTTPS") {
			console.ErrorExit(fmt.Errorf("application load balancer %s only supports HTTP or HTTPS", lb), "Invalid load balancer and protocol")
		}
	}

	o.LoadBalancerName = lb
	o.LoadBalancerArn = loadBalancer.Arn
}

func (o *ServiceCreateOperation) SetRules(inputRules []string) {
	var rules []ELBV2.Rule
	var msgs []string

	validRuleTypes := regexp.MustCompile(validRuleTypesPattern)

	if len(inputRules) > 0 && o.LoadBalancerArn == "" {
		msgs = append(msgs, "lb must be configured if rules are specified")
	}

	for _, inputRule := range inputRules {
		splitInputRule := strings.SplitN(inputRule, "=", 2)

		if len(splitInputRule) != 2 {
			msgs = append(msgs, "rules must be in the form of type=value")
		}

		if !validRuleTypes.MatchString(splitInputRule[0]) {
			msgs = append(msgs, fmt.Sprintf("Invalid rule type %s [must be path or host]", splitInputRule[0]))
		}

		rules = append(rules,
			ELBV2.Rule{
				Type:  strings.ToUpper(splitInputRule[0]),
				Value: splitInputRule[1],
			},
		)
	}

	if len(msgs) > 0 {
		console.ErrorExit(fmt.Errorf(strings.Join(msgs, ", ")), "Invalid rule")
	}

	o.Rules = rules
}

func (o *ServiceCreateOperation) SetEnvVars(inputEnvVars []string) {
	o.EnvVars = extractEnvVars(inputEnvVars)
}

var (
	flagServiceCreateCpu     string
	flagServiceCreateEnvVars []string
	flagServiceCreateImage   string
	flagServiceCreateLb      string
	flagServiceCreateMemory  string
	flagServiceCreatePort    string
	flagServiceCreateRules   []string
)

var serviceCreateCmd = &cobra.Command{
	Use:   "create <service name>",
	Short: "Create and deploy and new service",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		operation := &ServiceCreateOperation{
			ServiceName: args[0],
			Cpu:         flagServiceCreateCpu,
			Memory:      flagServiceCreateMemory,
			Image:       flagServiceCreateImage,
			Elbv2:       ELBV2.New(sess),
		}

		operation.Validate()
		operation.SetPort(flagServiceCreatePort)
		operation.SetLoadBalancer(flagServiceCreateLb)
		operation.SetRules(flagServiceCreateRules)
		operation.SetEnvVars(flagServiceCreateEnvVars)

		createService(operation)
	},
}

func init() {
	serviceCreateCmd.Flags().StringVarP(&flagServiceCreateCpu, "cpu", "c", "256", "Amount of cpu units to allocate for each task")
	serviceCreateCmd.Flags().StringVarP(&flagServiceCreateMemory, "memory", "m", "512", "Amount of MiB to allocate for each task")
	serviceCreateCmd.Flags().StringSliceVarP(&flagServiceCreateEnvVars, "env", "e", []string{}, "Environment variables to set [e.g. KEY=value]")
	serviceCreateCmd.Flags().StringVarP(&flagServiceCreatePort, "port", "p", "", "Port to listen on [e.g., 80, 443, http:8080, https:8443, tcp:1935]")
	serviceCreateCmd.Flags().StringVarP(&flagServiceCreateImage, "image", "i", "", "Docker image to run in the service; if omitted Fargate will build an image from the Dockerfile in the current directory")
	serviceCreateCmd.Flags().StringVarP(&flagServiceCreateLb, "lb", "l", "", "Name of a load balancer to use")
	serviceCreateCmd.Flags().StringSliceVarP(&flagServiceCreateRules, "rule", "r", []string{}, "Routing rule for the load balancer [e.g. host=api.example.com, path=/api/*]; if omitted service will be the default route")

	serviceCmd.AddCommand(serviceCreateCmd)
}

func createService(operation *ServiceCreateOperation) {
	console.Info("Creating %s", operation.ServiceName)

	cwl := CWL.New(sess)
	ec2 := EC2.New(sess)
	ecr := ECR.New(sess)
	ecs := ECS.New(sess)
	iam := IAM.New(sess)

	var (
		targetGroupArn string
		repositoryUri  string
	)

	if ecr.IsRepositoryCreated(operation.ServiceName) {
		repositoryUri = ecr.GetRepositoryUri(operation.ServiceName)
	} else {
		repositoryUri = ecr.CreateRepository(operation.ServiceName)
	}

	repository := docker.Repository{Uri: repositoryUri}
	subnetIds := ec2.GetDefaultVpcSubnetIds()
	ecsTaskExecutionRoleArn := iam.CreateEcsTaskExecutionRole()
	logGroupName := cwl.CreateLogGroup(logGroupFormat, operation.ServiceName)

	if operation.Image == "" {
		var tag string

		username, password := ecr.GetUsernameAndPassword()

		if git.IsCwdGitRepo() {
			tag = git.GetShortSha()
		} else {
			tag = docker.GenerateTag()
		}

		repository.Login(username, password)
		repository.Build(tag)
		repository.Push(tag)

		operation.Image = repository.UriFor(tag)
	}

	if operation.LoadBalancerArn != "" {
		vpcId := ec2.GetDefaultVpcId()
		targetGroupArn = operation.Elbv2.CreateTargetGroup(
			&ELBV2.CreateTargetGroupInput{
				Name:     operation.LoadBalancerName + "-" + operation.ServiceName,
				Port:     operation.Port.Port,
				Protocol: operation.Port.Protocol,
				VpcId:    vpcId,
			},
		)

		if len(operation.Rules) > 0 {
			for _, rule := range operation.Rules {
				operation.Elbv2.AddRule(operation.LoadBalancerArn, targetGroupArn, rule)
			}
		} else {
			operation.Elbv2.ModifyLoadBalancerDefaultAction(operation.LoadBalancerArn, targetGroupArn)
		}
	}

	taskDefinitionArn := ecs.CreateTaskDefinition(
		&ECS.CreateTaskDefinitionInput{
			Cpu:              operation.Cpu,
			EnvVars:          operation.EnvVars,
			ExecutionRoleArn: ecsTaskExecutionRoleArn,
			Image:            operation.Image,
			Memory:           operation.Memory,
			Name:             operation.ServiceName,
			Port:             operation.Port.Port,
			LogGroupName:     logGroupName,
			LogRegion:        region,
		},
	)

	ecs.CreateService(
		&ECS.CreateServiceInput{
			Cluster:           clusterName,
			Name:              operation.ServiceName,
			Port:              operation.Port.Port,
			SubnetIds:         subnetIds,
			TargetGroupArn:    targetGroupArn,
			TaskDefinitionArn: taskDefinitionArn,
		},
	)
}
