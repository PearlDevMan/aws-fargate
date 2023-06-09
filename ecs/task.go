package ecs

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	awsecs "github.com/aws/aws-sdk-go/service/ecs"
	"github.com/turnerlabs/fargate/console"
)

const (
	detailNetworkInterfaceId  = "networkInterfaceId"
	detailSubnetId            = "subnetId"
	startedByFormat           = "fargate:%s"
	taskGroupStartedByPattern = "fargate:(.*)"
	eniAttachmentType         = "ElasticNetworkInterface"
)

type Task struct {
	Cpu              string
	CreatedAt        time.Time
	DeploymentId     string
	DesiredStatus    string
	EniId            string
	EnvVars          []EnvVar
	Image            string
	LastStatus       string
	Memory           string
	SecurityGroupIds []string
	StartedBy        string
	SubnetId         string
	TaskId           string
	TaskRole         string
}

func (t *Task) RunningFor() time.Duration {
	return time.Now().Sub(t.CreatedAt).Truncate(time.Second)
}

type TaskGroup struct {
	TaskGroupName string
	Instances     int64
}

type RunTaskInput struct {
	ClusterName       string
	Count             int64
	SecurityGroupIds  []string
	SubnetIds         []string
	TaskDefinitionArn string
	TaskName          string
}

func (ecs *ECS) RunTask(i *RunTaskInput) {
	_, err := ecs.svc.RunTask(
		&awsecs.RunTaskInput{
			Cluster:        aws.String(i.ClusterName),
			Count:          aws.Int64(i.Count),
			TaskDefinition: aws.String(i.TaskDefinitionArn),
			LaunchType:     aws.String(awsecs.CompatibilityFargate),
			StartedBy:      aws.String(fmt.Sprintf(startedByFormat, i.TaskName)),
			NetworkConfiguration: &awsecs.NetworkConfiguration{
				AwsvpcConfiguration: &awsecs.AwsVpcConfiguration{
					AssignPublicIp: aws.String(awsecs.AssignPublicIpEnabled),
					Subnets:        aws.StringSlice(i.SubnetIds),
					SecurityGroups: aws.StringSlice(i.SecurityGroupIds),
				},
			},
		},
	)

	if err != nil {
		console.ErrorExit(err, "Could not run ECS task")
	}
}

func (ecs *ECS) DescribeTasksForService(serviceName string) []Task {
	return ecs.listTasks(
		&awsecs.ListTasksInput{
			Cluster:     aws.String(ecs.ClusterName),
			LaunchType:  aws.String(awsecs.CompatibilityFargate),
			ServiceName: aws.String(serviceName),
		},
	)
}

func (ecs *ECS) DescribeTasksForTaskGroup(taskGroupName string) []Task {
	return ecs.listTasks(
		&awsecs.ListTasksInput{
			StartedBy: aws.String(fmt.Sprintf(startedByFormat, taskGroupName)),
			Cluster:   aws.String(ecs.ClusterName),
		},
	)
}

func (ecs *ECS) ListTaskGroups() []*TaskGroup {
	var taskGroups []*TaskGroup

	taskGroupStartedByRegexp := regexp.MustCompile(taskGroupStartedByPattern)

	input := &awsecs.ListTasksInput{
		Cluster: aws.String(ecs.ClusterName),
	}

OUTER:
	for _, task := range ecs.listTasks(input) {
		matches := taskGroupStartedByRegexp.FindStringSubmatch(task.StartedBy)

		if len(matches) == 2 {
			taskGroupName := matches[1]

			for _, taskGroup := range taskGroups {
				if taskGroup.TaskGroupName == taskGroupName {
					taskGroup.Instances++
					continue OUTER
				}
			}

			taskGroups = append(
				taskGroups,
				&TaskGroup{
					TaskGroupName: taskGroupName,
					Instances:     1,
				},
			)
		}
	}

	return taskGroups
}

func (ecs *ECS) StopTasks(taskIds []string) {
	for _, taskId := range taskIds {
		ecs.StopTask(taskId)
	}
}

func (ecs *ECS) StopTask(taskId string) {
	_, err := ecs.svc.StopTask(
		&awsecs.StopTaskInput{
			Cluster: aws.String(ecs.ClusterName),
			Task:    aws.String(taskId),
		},
	)

	if err != nil {
		console.ErrorExit(err, "Could not stop ECS task")
	}
}

func (ecs *ECS) listTasks(input *awsecs.ListTasksInput) []Task {
	var tasks []Task
	var taskArnBatches [][]string

	err := ecs.svc.ListTasksPages(
		input,
		func(resp *awsecs.ListTasksOutput, lastPage bool) bool {
			if len(resp.TaskArns) > 0 {
				taskArnBatches = append(taskArnBatches, aws.StringValueSlice(resp.TaskArns))
			}

			return true
		},
	)

	if err != nil {
		console.ErrorExit(err, "Could not list ECS tasks")
	}

	if len(taskArnBatches) > 0 {
		for _, taskArnBatch := range taskArnBatches {
			for _, task := range ecs.DescribeTasks(taskArnBatch) {
				tasks = append(tasks, task)
			}
		}
	}

	return tasks
}

func (ecs *ECS) DescribeTasks(taskIds []string) []Task {
	var tasks []Task

	if len(taskIds) == 0 {
		return tasks
	}

	resp, err := ecs.svc.DescribeTasks(
		&awsecs.DescribeTasksInput{
			Cluster: aws.String(ecs.ClusterName),
			Tasks:   aws.StringSlice(taskIds),
		},
	)

	if err != nil {
		console.ErrorExit(err, "Could not describe ECS tasks")
	}

	for _, t := range resp.Tasks {
		taskArn := aws.StringValue(t.TaskArn)
		contents := strings.Split(taskArn, "/")
		taskID := contents[len(contents)-1]

		task := Task{
			Cpu:           aws.StringValue(t.Cpu),
			CreatedAt:     aws.TimeValue(t.CreatedAt),
			DeploymentId:  ecs.GetRevisionNumber(aws.StringValue(t.TaskDefinitionArn)),
			DesiredStatus: aws.StringValue(t.DesiredStatus),
			LastStatus:    aws.StringValue(t.LastStatus),
			Memory:        aws.StringValue(t.Memory),
			TaskId:        taskID,
			StartedBy:     aws.StringValue(t.StartedBy),
		}

		taskDefinition := ecs.DescribeTaskDefinition(aws.StringValue(t.TaskDefinitionArn))
		task.Image = aws.StringValue(taskDefinition.TaskDefinition.ContainerDefinitions[0].Image)
		task.TaskRole = aws.StringValue(taskDefinition.TaskDefinition.TaskRoleArn)

		for _, environment := range taskDefinition.TaskDefinition.ContainerDefinitions[0].Environment {
			task.EnvVars = append(
				task.EnvVars,
				EnvVar{
					Key:   aws.StringValue(environment.Name),
					Value: aws.StringValue(environment.Value),
				},
			)
		}

		found, eniId, subnetId := determineENIDetails(t)

		if found {
			task.EniId = eniId
			task.SubnetId = subnetId
		}

		tasks = append(tasks, task)
	}

	return tasks
}

func determineENIDetails(t *awsecs.Task) (bool, string, string) {
	foundEni := false
	var eniId, subnetId = "", ""

	//if there are attachments in the task details
	if len(t.Attachments) > 0 {
		for _, attachment := range t.Attachments {
			// only find network interface attachments
			if *attachment.Type != eniAttachmentType {
				continue
			}
			foundEni = true

			// pull out the details for the network interface
			for _, detail := range attachment.Details {
				switch aws.StringValue(detail.Name) {
				case detailNetworkInterfaceId:
					eniId = aws.StringValue(detail.Value)
				case detailSubnetId:
					subnetId = aws.StringValue(detail.Value)
				}
			}
		}
	}

	return foundEni, eniId, subnetId
}
