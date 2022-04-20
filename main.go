// SPDX-License-Identifier: MIT

package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/daimler/kosmoo/pkg/metrics"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/utils/openstack/clientconfig"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

var (
	refreshInterval = flag.Int64("refresh-interval", 120, "Interval between scrapes to OpenStack API (default 120s)")
	addr            = flag.String("addr", ":9183", "Address to listen on")
	osCloud         = flag.String("cloud", "", "Which cloud (from clouds.yaml) should be used. If this is not set the scraper will try to use the usual OpenStack environment variables.")
	kubeconfig      = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "Path to the kubeconfig file to use for CLI requests. (uses in-cluster config if empty)")
	metricsPrefix   = flag.String("metrics-prefix", metrics.DefaultMetricsPrefix, "Prefix used for all metrics")
)

var (
	scrapeDuration *prometheus.GaugeVec
	scrapedAt      *prometheus.GaugeVec
	scrapedStatus  *prometheus.GaugeVec
)

var (
	clientset       *kubernetes.Clientset
	backoffSleep    = time.Second
	maxBackoffSleep = time.Hour
)

func registerMetrics(prefix string) {
	scrapeDuration = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metrics.AddPrefix("scrape_duration", prefix),
			Help: "Time in seconds needed for the last scrape",
		},
		[]string{"refresh_interval"},
	)
	scrapedAt = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metrics.AddPrefix("scraped_at", prefix),
			Help: "Timestamp when last scrape started",
		},
		[]string{"refresh_interval"},
	)
	scrapedStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metrics.AddPrefix("scrape_status_succeeded", prefix),
			Help: "Scrape status succeeded",
		},
		[]string{"refresh_interval"},
	)

	prometheus.MustRegister(scrapeDuration)
	prometheus.MustRegister(scrapedAt)
	prometheus.MustRegister(scrapedStatus)
}

// metrixMutex locks to prevent race-conditions between scraping the metrics
// endpoint and updating the metrics
var metricsMutex sync.Mutex

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	klog.Infof("starting kosmoo at %s", *addr)

	registerMetrics(*metricsPrefix)
	metrics.RegisterMetrics(*metricsPrefix)

	// start prometheus metrics endpoint
	go func() {
		// klog.Info().Str("addr", *addr).Msg("starting prometheus http endpoint")
		klog.Infof("starting prometheus http endpoint at %s", *addr)
		metricsMux := http.NewServeMux()
		promHandler := promhttp.Handler()
		metricsMux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			// metricsMutex ensures that we do not drop the metrics during promHandler.ServeHTTP call
			metricsMutex.Lock()
			defer metricsMutex.Unlock()
			promHandler.ServeHTTP(w, r)
		})

		metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte(http.StatusText(http.StatusOK))); err != nil {
				klog.Warningf("error handling /healthz: %v", err)
			}
		})

		err := http.ListenAndServe(*addr, metricsMux)
		klog.Fatalf("prometheus http.ListenAndServe failed: %v", err)
	}()

	for {
		if run() != nil {
			klog.Errorf("error during run - sleeping %s", backoffSleep)
			time.Sleep(backoffSleep)
			backoffSleep = min(2*backoffSleep, maxBackoffSleep)
		}
	}
}

// run does the initialization of the operational exporter and also the metrics scraping
func run() error {
	var authOpts gophercloud.AuthOptions
	var endpointOpts gophercloud.EndpointOpts
	var err error

	// get OpenStack credentials
	if *osCloud != "" {
		// This will try to load auth options for the given cloud in a clouds.yaml file
		// in all the usual locations.
		// See https://github.com/gophercloud/utils/blob/8e7800759d1638be1f2404f34a44109a502f19f2/openstack/clientconfig/utils.go#L96-L119
		ao, err := clientconfig.AuthOptions(&clientconfig.ClientOpts{
			Cloud: *osCloud,
		})
		if err != nil {
			return logError("unable to read OpenStack credentials from clouds.yaml: %v", err)
		}
		authOpts = *ao
		klog.Infof("OpenStack credentials read from clouds.yaml")
	} else {
		authOpts, err = openstack.AuthOptionsFromEnv()
		if err != nil {
			return logError("unable to get authentication credentials from environment: %v", err)
		}
		endpointOpts = gophercloud.EndpointOpts{
			Region: os.Getenv("OS_REGION_NAME"),
		}
		if endpointOpts.Region == "" {
			endpointOpts.Region = "nova"
		}
		klog.Info("OpenStack credentials read from environment")
	}

	// authenticate to OpenStack
	provider, err := openstack.AuthenticatedClient(authOpts)
	if err != nil {
		return logError("unable to authenticate to OpenStack: %v", err)
	}
	klog.Infof("OpenStack authentication was successful")

	// get kubernetes clientset
	var config *rest.Config
	config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return logError("unable to get kubernetes config: %v", err)
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		return logError("error creating kubernetes Clientset: %v", err)
	}

	// start scraping loop
	for {
		err := updateMetrics(provider, endpointOpts, clientset, authOpts.TenantID)
		if err != nil {
			return err
		}
		time.Sleep(time.Second * time.Duration(*refreshInterval))
	}
}

func updateMetrics(provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts, clientset *kubernetes.Clientset, tenantID string) error {
	metricsMutex.Lock()
	defer metricsMutex.Unlock()

	var errs []error
	scrapeStart := time.Now()

	cinderClient, neutronClient, loadbalancerClient, computeClient, err := getClients(provider, eo)
	if err != nil {
		err := logError("creating openstack clients failed: %v", err)
		errs = append(errs, err)
	} else {
		if err := metrics.PublishCinderMetrics(cinderClient, clientset, tenantID); err != nil {
			err := logError("scraping cinder metrics failed: %v", err)
			errs = append(errs, err)
		}

		if err := metrics.PublishNeutronMetrics(neutronClient, tenantID); err != nil {
			err := logError("scraping neutron metrics failed: %v", err)
			errs = append(errs, err)
		}

		if err := metrics.PublishLoadBalancerMetrics(loadbalancerClient, tenantID); err != nil {
			err := logError("scraping load balancer metrics failed: %v", err)
			errs = append(errs, err)
		}

		if err := metrics.PublishServerMetrics(computeClient, tenantID); err != nil {
			err := logError("scraping server metrics failed: %v", err)
			errs = append(errs, err)
		}

		if err := metrics.PublishFirewallV1Metrics(neutronClient, tenantID); err != nil {
			err := logError("scraping firewall v1 metrics failed: %v", err)
			errs = append(errs, err)
		}

		if err := metrics.PublishFirewallV2Metrics(neutronClient, tenantID); err != nil {
			err := logError("scraping firewall v1 metrics failed: %v", err)
			errs = append(errs, err)
		}
	}

	duration := time.Since(scrapeStart)
	scrapeDuration.WithLabelValues(
		fmt.Sprintf("%d", *refreshInterval),
	).Set(duration.Seconds())

	scrapedAt.WithLabelValues(
		fmt.Sprintf("%d", *refreshInterval),
	).Set(float64(scrapeStart.Unix()))

	if len(errs) > 0 {
		scrapedStatus.WithLabelValues(
			fmt.Sprintf("%d", *refreshInterval),
		).Set(0)
		return fmt.Errorf("errors during scrape loop")
	}

	scrapedStatus.WithLabelValues(
		fmt.Sprintf("%d", *refreshInterval),
	).Set(1)

	// reset backoff after successful scrape
	backoffSleep = time.Second

	return nil
}

func getClients(provider *gophercloud.ProviderClient, endpointOpts gophercloud.EndpointOpts) (cinder, neutron, loadbalancer, compute *gophercloud.ServiceClient, err error) {
	cinderClient, err := openstack.NewBlockStorageV2(provider, endpointOpts)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("unable to get cinder client: %v", err)
	}

	neutronClient, err := openstack.NewNetworkV2(provider, endpointOpts)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("unable to get neutron client: %v", err)
	}

	computeClient, err := openstack.NewComputeV2(provider, endpointOpts)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("unable to get compute client: %v", err)
	}

	if _, err := provider.EndpointLocator(gophercloud.EndpointOpts{Type: "load-balancer", Availability: gophercloud.AvailabilityPublic}); err != nil {
		// we can use the neutron client to access lbaas because no octavia is available
		return cinderClient, neutronClient, neutronClient, computeClient, nil
	}

	loadbalancerClient, err := openstack.NewLoadBalancerV2(provider, endpointOpts)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to create loadbalancer service client: %v", err)
	}
	return cinderClient, neutronClient, loadbalancerClient, computeClient, nil
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func logError(format string, a ...interface{}) error {
	err := fmt.Errorf(format, a...)
	klog.Error(err)
	return err
}
