/*
Copyright (c) 2019 TriggerMesh, Inc

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/actions/workflow-parser/model"
	"github.com/spf13/cobra"

	"github.com/triggermesh/aktion/pkg/client"

	pipeline "github.com/knative/build-pipeline/pkg/apis/pipeline/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	event                   string
	registry                string
	taskrun                 bool
	visitedActionDependency map[string]bool
	applyPipelineFlag       bool
	kubeNamespace           string
)

//Task represents Task object
type Task struct {
	Identifier string
	Image      string
	Cmd        []string
	Args       []string
	Envs       []corev1.EnvVar
	EnvFrom    []corev1.EnvFromSource
}

//Tasks groups Task objects by one Identifier
type Tasks struct {
	Identifier string
	Task       []Task
}

//NewCreateCmd creates new create command
func NewCreateCmd(kubeConfig *string, ns *string, repository *string) *cobra.Command {
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Convert the Github Action workflow into a Tekton Task list",
		Run: func(cmd *cobra.Command, args []string) {
			config := ParseData()
			visitedActionDependency = make(map[string]bool)
			namespace = *ns
			repo = *repository

			if repo != "" {
				repoPipeline := createPipelineResource(repo, config)

				fmt.Printf("%s", GenerateObjBreak(true))
				fmt.Print(GenerateOutput(repoPipeline))
				fmt.Printf("%s", GenerateObjBreak(false))
			}

			for _, act := range config.Workflows {
				taskRun := CreateTaskRun(act.Identifier)
				tasks := extractTasks(act.Identifier, config)

				if applyPipelineFlag {
					applyPipeline(*kubeConfig, taskRun, CreateTask(tasks, repo))
				} else {
					fmt.Printf("%s", GenerateOutput(CreateTask(tasks, repo)))

					if taskrun {
						fmt.Printf("%s", GenerateObjBreak(false))
						fmt.Printf("%s", GenerateOutput(taskRun))
					}
				}
			}

			fmt.Printf("%s", GenerateObjLastBreak())
		},
	}

	createCmd.Flags().StringVarP(&registry, "registry", "r", "knative.registry.svc.cluster.local", "Default docker registry")
	createCmd.Flags().BoolVarP(&taskrun, "taskrun", "t", false, "Flag to create TaskRun")
	createCmd.Flags().BoolVarP(&applyPipelineFlag, "apply", "a", false, "Apply the generated Tekton pipeline to the user's kubernetes cluster")

	return createCmd
}

func applyPipeline(kubeConfig string, taskRun pipeline.TaskRun, tasks pipeline.Task) {
	// add if check for taskrun to build/inject the task
	clientSet, err := client.NewClient(client.ConfigPath(kubeConfig))
	if err != nil {
		Panic("Error connecting to kubernetes cluster: %s\n", err)
	}

	_, err = clientSet.Pipeline.TektonV1alpha1().Tasks(namespace).Create(&tasks)
	if err != nil {
		Panic("Unable to create tasks: %s\n", err)
	}

	if taskrun {
		_, err = clientSet.Pipeline.TektonV1alpha1().TaskRuns(namespace).Create(&taskRun)
		if err != nil {
			Panic("Unable to create task run: %s\n", err)
		}
	}
}

func extractTasks(name string, config *model.Configuration) Tasks {
	tasks := Tasks{
		Identifier: name,
		Task:       make([]Task, 0),
	}
	workflow := config.GetWorkflow(name)

	extractedTasks := make([]Task, 0)
	for _, a := range workflow.Resolves {
		extractedTasks = append(extractedTasks, extractActions(config.GetAction(a), config)...)
	}
	tasks.Task = extractedTasks

	return tasks
}

func extractActions(action *model.Action, config *model.Configuration) []Task {
	tasks := make([]Task, 0)

	if len(action.Needs) > 0 {
		for _, a := range action.Needs {
			if !visitedActionDependency[config.GetAction(a).Identifier] {
				tasks = append(tasks, extractActions(config.GetAction(a), config)...)
			}
		}
	}

	if action.Uses == nil {
		return tasks
	}

	task := Task{
		Identifier: action.Identifier,
	}

	if !strings.HasPrefix(action.Uses.String(), "docker://") {
		Panic("Can only support docker images for now.\n")
	}

	task.Image = strings.TrimPrefix(action.Uses.String(), "docker://")

	if action.Runs != nil {
		task.Cmd = action.Runs.Split()
	}

	if action.Args != nil {
		task.Args = action.Args.Split()
	}

	task.Envs = make([]corev1.EnvVar, 0)
	for k, v := range action.Env {
		env := corev1.EnvVar{
			Name:  k,
			Value: v,
		}

		task.Envs = append(task.Envs, env)
	}

	if action.Secrets != nil {
		task.EnvFrom = make([]corev1.EnvFromSource, 0)
		for _, s := range action.Secrets {
			secret := corev1.EnvFromSource{
				SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: s}},
			}
			task.EnvFrom = append(task.EnvFrom, secret)
		}
	}

	// Mark as visited
	visitedActionDependency[task.Identifier] = true

	return append(tasks, task)
}

//CreateTaskRun function creates TaskRun object
func CreateTaskRun(name string) pipeline.TaskRun {
	taskRun := pipeline.TaskRun{
		Spec: pipeline.TaskRunSpec{
			TaskRef: &pipeline.TaskRef{
				Name: convertName(name),
			},
			Trigger: pipeline.TaskTrigger{
				Type: pipeline.TaskTriggerTypeManual,
			},
		},
	}

	taskRun.SetDefaults()
	taskRun.TypeMeta = metav1.TypeMeta{
		Kind:       "TaskRun",
		APIVersion: "tekton.dev/v1alpha1",
	}

	taskRun.ObjectMeta = metav1.ObjectMeta{
		Name:              convertName(name),
		CreationTimestamp: metav1.Time{time.Now()},
	}

	err := taskRun.Validate()
	if err != nil {
		Panic("Failed validation: %s\n", err)
	}

	return taskRun
}

//CreateTask creates Task object
func CreateTask(tasks Tasks, repo string) pipeline.Task {
	task := pipeline.Task{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Task",
			APIVersion: "tekton.dev/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: convertName(tasks.Identifier),
		},
	}

	var taskSpec pipeline.TaskSpec
	steps := make([]corev1.Container, 0)

	if repo != "" {
		taskSpec.Inputs = &pipeline.Inputs{
			Resources: []pipeline.TaskResource{{
				Name: convertName(tasks.Identifier),
				Type: "git",
			}},
		}
	}

	for _, t := range tasks.Task {
		steps = append(steps, createContainer(t))
	}
	taskSpec.Steps = steps
	task.Spec = taskSpec

	return task
}

func createPipelineResource(repo string, config *model.Configuration) pipeline.PipelineResource {

	// Hack: Get the first worklow in the list to get a name
	workflow := config.Workflows[0]
	resource := pipeline.PipelineResource{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PipelineResource",
			APIVersion: "tekton.dev/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: convertName(workflow.Identifier),
		},
	}

	inputparams := make([]pipeline.Param, 0)

	inputparams = append(inputparams, pipeline.Param{
		Name:  "revision",
		Value: "master",
	})

	inputparams = append(inputparams, pipeline.Param{
		Name:  "url",
		Value: repo,
	})

	resourcespec := pipeline.PipelineResourceSpec{
		Type:   "git",
		Params: inputparams,
	}
	resource.Spec = resourcespec
	return resource
}

func createContainer(task Task) corev1.Container {
	return corev1.Container{
		Name:    convertName(task.Identifier),
		Image:   task.Image,
		Command: task.Cmd,
		Args:    task.Args,
		Env:     task.Envs,
		EnvFrom: task.EnvFrom,
	}
}

func convertName(name string) string {
	n := strings.Replace(name, " ", "-", -1)
	return strings.ToLower(n)
}
