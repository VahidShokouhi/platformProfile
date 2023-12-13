package main

import (
	"context"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	kubeClient *kubernetes.Clientset
	nodesMutex sync.Mutex
	nodes      []*v1.Node
)

func init() {
	configPath := filepath.Join(".", "config")
	config, err := clientcmd.BuildConfigFromFlags("", configPath)
	if err != nil {
		log.Fatalf("Error loading kubeconfig: %v", err)
	}

	kubeClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error creating Kubernetes client: %v", err)
	}
}

func main() {
	r := gin.Default()
	r.GET("/nodeinfo/v1/nodes/jupyter/profiles", getNodeProfiles)
	go watchNodes("scope=platform,type=gpu,model=1030") // Update the label selector here
	r.Run("0.0.0.0:8184")
}

func watchNodes(labelSelector string) {
	nodeListWatcher := cache.NewListWatchFromClient(
		kubeClient.CoreV1().RESTClient(),
		"nodes",
		v1.NamespaceAll,
		fields.Everything(),
	)

	_, controller := cache.NewInformer(
		nodeListWatcher,
		&v1.Node{},
		time.Second*0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				node, ok := obj.(*v1.Node)
				if !ok {
					return
				}
				nodesMutex.Lock()
				nodes = append(nodes, node)
				nodesMutex.Unlock()
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				node, ok := newObj.(*v1.Node)
				if !ok {
					return
				}
				nodesMutex.Lock()
				nodes = append(nodes, node)
				nodesMutex.Unlock()
			},
		},
	)

	stopCh := make(chan struct{})
	defer close(stopCh)

	go controller.Run(stopCh)

	time.Sleep(time.Second * 10)

	nodesMutex.Lock()
	defer nodesMutex.Unlock()

	log.Println("Node Labels:")
	for _, node := range nodes {
		log.Printf("Node Name: %s, Labels: %v", node.Name, node.Labels)
	}
}

func getNodeProfiles(c *gin.Context) {
	nodesMutex.Lock()
	nodesCopy := make([]*v1.Node, len(nodes))
	copy(nodesCopy, nodes)
	nodesMutex.Unlock()

	counts := make(map[string]map[string]int)

	for _, node := range nodesCopy {
		model := node.Labels["model"]
		scope := node.Labels["scope"]

		if model == "" || scope == "" {
			continue
		}

		// Check if the "platform" keyword is present in the scope
		if scope == "platform" {
			if _, ok := counts[model]; !ok {
				counts[model] = make(map[string]int)
			}

			describedNode, err := kubeClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
			if err != nil {
				log.Printf("Error describing node %s: %v", node.Name, err)
				continue
			}

			if gpuStr, exists := describedNode.Status.Allocatable[v1.ResourceName("nvidia.com/gpu")]; exists {
				gpuCount, err := strconv.Atoi(gpuStr.String())
				if err != nil {
					log.Printf("Error converting GPU count: %v", err)
					continue
				}

				podList, err := kubeClient.CoreV1().Pods(v1.NamespaceAll).List(context.TODO(), metav1.ListOptions{
					FieldSelector: "spec.nodeName=" + node.Name,
				})
				if err != nil {
					log.Printf("Error listing pods on node %s: %v", node.Name, err)
					continue
				}

				for _, pod := range podList.Items {
					for _, container := range pod.Spec.Containers {
						if gpuQuantity, found := container.Resources.Limits[v1.ResourceName("nvidia.com/gpu")]; found {
							gpuCount -= int(gpuQuantity.Value())
						}
					}
				}

				if gpuCount > 0 {
					counts[model]["used"]++
				} else {
					counts[model]["free"]++
				}
			}
		}
	}

	var profiles []gin.H
	for model, count := range counts {
		profileData := gin.H{
			"model": model,
			"scope": "platform",
			"used":  count["free"],
			"free":  count["used"],
		}
		profiles = append(profiles, profileData)
	}

	c.JSON(http.StatusOK, profiles)
}
