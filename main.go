/*
Copyright 2017 The Kubernetes Authors.

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

package main

import (
	// "flag"
	"fmt"
	// "reflect"
	"bytes"
	"context"
	"io"
	"os"
	// "errors"
	"regexp"
	"text/tabwriter"
	"time"

	"k8s.io/klog/v2"

	// "github.com/go-telegram-bot-api/telegram-bot-api"

	"github.com/spf13/pflag"
	// "k8s.io/cli-runtime/pkg/genericclioptions"

	v1 "k8s.io/api/core/v1"
	// meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	delay                 int64
	chatID                int64
	tailLines             *int64
	namespace             string
	podNamePatterns       []string
	containerNamePatterns []string

	version, commitID string

	clientset *kubernetes.Clientset
)

type Controller struct {
	indexer  cache.Indexer
	queue    workqueue.RateLimitingInterface
	informer cache.Controller
}

func NewController(queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller) *Controller {
	return &Controller{
		informer: informer,
		indexer:  indexer,
		queue:    queue,
	}
}

func (c *Controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer c.queue.Done(key)

	// Invoke the method containing the business logic
	// err := c.syncToStdout(key.(string))
	err := c.syncState(key.(string))
	// Handle the error if something went wrong during the execution of the business logic
	c.handleErr(err, key)
	return true
}

// syncToStdout is the business logic of the controller. In this controller it simply prints
// information about the pod to stdout. In case an error happened, it has to simply return the error.
// The retry logic should not be part of the business logic.
// func (c *Controller) syncToStdout(key string) error {
func (c *Controller) syncState(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		klog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		fmt.Printf("Pod %s does not exist anymore\n", key)
	} else {
		// Note that you also have to check the uid if you have a local controlled resource, which
		// is dependent on the actual instance, to detect that a Pod was recreated with the same name
		go processPod(obj)
	}
	return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(key) < 5 {
		klog.Infof("Error syncing pod %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	klog.Infof("Dropping pod %q out of the queue: %v", key, err)
}

func (c *Controller) Run(threadiness int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	// Let the workers stop when we are done
	defer c.queue.ShutDown()
	klog.Info("Starting Pod controller")

	go c.informer.Run(stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	klog.Info("Stopping Pod controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

func main() {
	var config *rest.Config
	var kubeconfig string
	var err error
	var versionFlag bool

	pflag.BoolVar(&versionFlag, "version", false, "return application version")
	pflag.Int64Var(&delay, "delay", 60, "delay between localtime and time in pod status field")
	pflag.Int64Var(&chatID, "chat-id", 0, "telegram chat id")
	pflag.StringVar(&kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "absolute path to the kubeconfig file")
	pflag.StringVar(&namespace, "namespace", "default", "monitored namespace")
	pflag.StringArrayVar(&podNamePatterns, "pod-name-pattern", []string{}, "pod name pattern(may be regexp), which will be monitored")
	pflag.StringArrayVar(&containerNamePatterns, "container-name-pattern", []string{}, "container name pattern(may be regexp), which will be monitored")

	tailLines = pflag.Int64("tail", 100000, "tail last num lines")

	pflag.Parse()

	if versionFlag {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		if version != "" {
			fmt.Fprintf(w, "version:\t%s\n", version)
		}
		fmt.Fprintf(w, "git commit:\t%s\n", commitID)
		w.Flush()
		os.Exit(0)
	}

	// creates the connection
	if len(kubeconfig) > 0 {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		klog.Fatal(err)
	}

	// creates the clientset
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatal(err)
	}

	// create the pod watcher
	// podListWatcher := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "pods", v1.NamespaceDefault, fields.Everything())
	podListWatcher := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "pods", namespace, fields.Everything())

	// create the workqueue
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	// Bind the workqueue to a cache with the help of an informer. This way we make sure that
	// whenever the cache is updated, the pod key is added to the workqueue.
	// Note that when we finally process the item from the workqueue, we might see a newer version
	// of the Pod than the version which was responsible for triggering the update.
	indexer, informer := cache.NewIndexerInformer(podListWatcher, &v1.Pod{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// IndexerInformer uses a delta queue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
	}, cache.Indexers{})

	controller := NewController(queue, indexer, informer)

	// Now let's start the controller
	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(1, stop)

	// Wait forever
	select {}
}

func sendContainerLogs(pod *v1.Pod, containerName string) error {
	podLogOpts := v1.PodLogOptions{
		Container: containerName,
		TailLines: tailLines,
	}

	req := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &podLogOpts)
	podLogs, err := req.Stream(context.TODO())
	if err != nil {
		return fmt.Errorf("[sendContainerLogs] failed create stream: %s", err)
	}
	defer podLogs.Close()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, podLogs)
	if err != nil {
		return fmt.Errorf("[sendContainerLogs] failed copy pod logs to buffer: %s", err)
	}

	prefix := fmt.Sprintf("%s_%s", pod.GetName(), containerName)

	err = sendLogsToTelegram(chatID, buf, prefix)
	if err != nil {
		return fmt.Errorf("[sendContainerLogs] failed send message: %s", err)
	}

	return nil
}

func isShouldCheck(name string, list []string) bool {
	if len(list) == 0 {
		return true
	} else {
		for _, n := range list {
			if n == name {
				return true
			}
		}
	}

	return false
}

func isPodShouldCheck(podName string, podList []string) bool {
	if len(podList) == 0 {
		return true
	} else {
		for _, pod := range podList {
			if matched, _ := regexp.MatchString(pod, podName); matched {
				return true
			}
		}
	}

	return false
}

func isContainerShouldCheck(containerName string, containerList []string) bool {
	return isShouldCheck(containerName, containerList)
}

func isContainerLogShouldSended(containerStatus v1.ContainerStatus) bool {
	containerState := containerStatus.State
	if containerState.Terminated != nil {
		startedAt := containerState.Terminated.StartedAt.Unix()
		finishedAt := containerState.Terminated.FinishedAt.Unix()

		now := time.Now().Unix()

		if (startedAt < finishedAt) && ((now - finishedAt) < delay) {
			return true
		}
	}

	return false
}

func processContainers(pod *v1.Pod) {
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if isContainerShouldCheck(containerStatus.Name, containerNamePatterns) {
			if isContainerLogShouldSended(containerStatus) {
				klog.Infof("Send logs from pod: %s, container: %s", pod.GetName(), containerStatus.Name)

				err := sendContainerLogs(pod, containerStatus.Name)
				if err != nil {
					klog.Errorf("[processContainers] failed sed contianer logs: %s", err)
				}
			}
		}
	}
}

func processPod(obj interface{}) {
	pod := obj.(*v1.Pod)

	podName := pod.GetName()

	klog.Infof("Event from pod: %s", podName)

	if isPodShouldCheck(podName, podNamePatterns) {
		processContainers(pod)
	}
}
