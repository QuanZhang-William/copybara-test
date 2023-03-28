package mutatingwebhook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/tektoncd/pipeline/pkg/workspace"
	"go.uber.org/zap"
	"gomodules.xyz/jsonpatch/v2"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	admissionlisters "k8s.io/client-go/listers/admissionregistration/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	kubeclient "knative.dev/pkg/client/injection/kube/client"
	mwhinformer "knative.dev/pkg/client/injection/kube/informers/admissionregistration/v1/mutatingwebhookconfiguration"
	"knative.dev/pkg/kmp"
	"knative.dev/pkg/ptr"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"knative.dev/pkg/controller"
	secretinformer "knative.dev/pkg/injection/clients/namespacedkube/informers/core/v1/secret"
	"knative.dev/pkg/logging"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/pkg/system"
	"knative.dev/pkg/webhook"
	certresources "knative.dev/pkg/webhook/certificates/resources"
)

const (
// PodAffinityName = "one-worker/custom-affinity"
)

var (
	scheme = runtime.NewScheme()
	codecs = serializer.NewCodecFactory(scheme)
)

// NewAdmissionController constructs a reconciler
func NewAdmissionController(
	ctx context.Context,
	name, path string,
	wc func(context.Context) context.Context,
) *controller.Impl {

	client := kubeclient.Get(ctx)
	mwhInformer := mwhinformer.Get(ctx)
	secretInformer := secretinformer.Get(ctx)
	options := webhook.GetOptions(ctx)

	key := types.NamespacedName{Name: name}

	wh := &reconciler{
		LeaderAwareFuncs: pkgreconciler.LeaderAwareFuncs{
			// Have this reconciler enqueue our singleton whenever it becomes leader.
			PromoteFunc: func(bkt pkgreconciler.Bucket, enq func(pkgreconciler.Bucket, types.NamespacedName)) error {
				enq(bkt, key)
				return nil
			},
		},
		key:          key,
		path:         path,
		withContext:  wc,
		secretName:   options.SecretName,
		client:       client,
		mwhlister:    mwhInformer.Lister(),
		secretlister: secretInformer.Lister(),
	}

	logger := logging.FromContext(ctx)
	const queueName = "MutatingWebhook"
	c := controller.NewContext(ctx, wh, controller.ControllerOptions{WorkQueueName: queueName, Logger: logger.Named(queueName)})

	// Reconcile when the named MutatingWebhookConfiguration changes.
	mwhInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controller.FilterWithName(name),
		// It doesn't matter what we enqueue because we will always Reconcile
		// the named MWH resource.
		Handler: controller.HandleAll(c.Enqueue),
	})

	// Reconcile when the cert bundle changes.
	secretInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controller.FilterWithNameAndNamespace(system.Namespace(), wh.secretName),
		// It doesn't matter what we enqueue because we will always Reconcile
		// the named MWH resource.
		Handler: controller.HandleAll(c.Enqueue),
	})

	return c
}

// reconciler implements the AdmissionController for resources
type reconciler struct {
	webhook.StatelessAdmissionImpl
	pkgreconciler.LeaderAwareFuncs
	key          types.NamespacedName
	path         string
	withContext  func(context.Context) context.Context
	client       kubernetes.Interface
	mwhlister    admissionlisters.MutatingWebhookConfigurationLister
	secretlister corelisters.SecretLister
	secretName   string
}

var _ controller.Reconciler = (*reconciler)(nil)
var _ pkgreconciler.LeaderAware = (*reconciler)(nil)
var _ webhook.AdmissionController = (*reconciler)(nil)
var _ webhook.StatelessAdmissionController = (*reconciler)(nil)

func (ac *reconciler) Reconcile(ctx context.Context, key string) error {
	logger := logging.FromContext(ctx)

	if !ac.IsLeaderFor(ac.key) {
		return controller.NewSkipKey(key)
	}

	// Look up the webhook secret, and fetch the CA cert bundle.
	secret, err := ac.secretlister.Secrets(system.Namespace()).Get(ac.secretName)
	if err != nil {
		logger.Errorw("Error fetching secret", zap.Error(err))
		return err
	}
	caCert, ok := secret.Data[certresources.CACert]
	if !ok {
		return fmt.Errorf("secret %q is missing %q key", ac.secretName, certresources.CACert)
	}

	// Reconcile the webhook configuration.
	return ac.reconcileMutatingWebhook(ctx, caCert)
}

func (ac *reconciler) reconcileMutatingWebhook(ctx context.Context, caCert []byte) error {
	logger := logging.FromContext(ctx)
	rules := []admissionregistrationv1.RuleWithOperations{{
		Operations: []admissionregistrationv1.OperationType{
			admissionregistrationv1.Create,
		},
		Rule: admissionregistrationv1.Rule{
			APIGroups:   []string{""},
			APIVersions: []string{"v1"},
			Resources:   []string{"pods"},
		},
	}}

	configuredWebhook, err := ac.mwhlister.Get(ac.key.Name)
	if err != nil {
		return fmt.Errorf("error retrieving webhook: %w", err)
	}

	current := configuredWebhook.DeepCopy()

	ns, err := ac.client.CoreV1().Namespaces().Get(ctx, system.Namespace(), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to fetch namespace: %w", err)
	}
	nsRef := *metav1.NewControllerRef(ns, corev1.SchemeGroupVersion.WithKind("Namespace"))
	current.OwnerReferences = []metav1.OwnerReference{nsRef}

	for i, wh := range current.Webhooks {
		if wh.Name != current.Name {
			continue
		}

		cur := &current.Webhooks[i]
		cur.Rules = rules

		cur.NamespaceSelector = webhook.EnsureLabelSelectorExpressions(
			cur.NamespaceSelector,
			&metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "webhooks.knative.dev/exclude",
					Operator: metav1.LabelSelectorOpDoesNotExist,
				}},
			})

		cur.ClientConfig.CABundle = caCert
		if cur.ClientConfig.Service == nil {
			return fmt.Errorf("missing service reference for webhook: %s", wh.Name)
		}
		cur.ClientConfig.Service.Path = ptr.String(ac.Path())

		cur.ReinvocationPolicy = ptrReinvocationPolicyType(admissionregistrationv1.IfNeededReinvocationPolicy)
	}

	if ok, err := kmp.SafeEqual(configuredWebhook, current); err != nil {
		return fmt.Errorf("error diffing webhooks: %w", err)
	} else if !ok {
		logger.Info("Updating webhook")
		mwhclient := ac.client.AdmissionregistrationV1().MutatingWebhookConfigurations()
		if _, err := mwhclient.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to update webhook: %w", err)
		}
	} else {
		logger.Info("Webhook is valid")
	}
	return nil
}

// Admit implements AdmissionController
// here we modify the pod affinity
func (ac *reconciler) Admit(ctx context.Context, request *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if ac.withContext != nil {
		ctx = ac.withContext(ctx)
	}

	logger := logging.FromContext(ctx)
	logger.Infof("Quan Test, in admission webhook, request is: %v \n", request)

	// convert the admission request to a pod
	gvkPod := corev1.SchemeGroupVersion.WithKind("Pod")
	var pod corev1.Pod
	codecs.UniversalDeserializer().Decode(request.Object.Raw, &gvkPod, &pod)
	cpPod := pod.DeepCopy()

	// mutate the pod only when it is created by a pipelinerun
	if pr, found := pod.Labels["tekton.dev/pipelineRun"]; found {
		mutatePodAffinity(ctx, &pod, pr)
	}

	// try patch a label for testing purpose
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels["QuanTest"] = "hello1"

	jp := generateJsonPatch(cpPod, &pod)

	return &admissionv1.AdmissionResponse{
		Patch:   jp,
		Allowed: true,
		PatchType: func() *admissionv1.PatchType {
			pt := admissionv1.PatchTypeJSONPatch
			return &pt
		}(),
	}
}

// Path implements AdmissionController
func (ac *reconciler) Path() string {
	return ac.path
}

func ptrReinvocationPolicyType(r admissionregistrationv1.ReinvocationPolicyType) *admissionregistrationv1.ReinvocationPolicyType {
	return &r
}

func generateJsonPatch(origin, target *corev1.Pod) []byte {
	targetBytes := new(bytes.Buffer)
	json.NewEncoder(targetBytes).Encode(target)

	originBytes := new(bytes.Buffer)
	json.NewEncoder(originBytes).Encode(origin)

	patch, e := jsonpatch.CreatePatch(originBytes.Bytes(), targetBytes.Bytes())
	if e != nil {
		fmt.Printf("error: %v", e)
	}

	bytes, err := json.Marshal(patch)
	if err != nil {
		fmt.Printf("error marshalling patch: %v", err)
	}

	return bytes
}

func mutatePodAffinity(ctx context.Context, p *corev1.Pod, pipelineRunName string) {
	if _, found := p.Annotations[workspace.AnnotationAffinityAssistantName]; found {
		logger := logging.FromContext(ctx)
		logger.Infof("exit mutatePodAffinity since Affinity Assistant exists")
		return
	}

	// for now we assume the original pod has no pod affinity
	if p.Spec.Affinity == nil {
		p.Spec.Affinity = &corev1.Affinity{}
	}

	podAffinityName := getPodAffinityValue(pipelineRunName)
	mergeAffinityWithAffinityAssistant(p.Spec.Affinity, podAffinityName)
}

func mergeAffinityWithAffinityAssistant(affinity *corev1.Affinity, podAffinityName string) {

	podAffinityTerm := podAffinityTermUsingAffinityAssistant(podAffinityName)

	if affinity.PodAffinity == nil {
		affinity.PodAffinity = &corev1.PodAffinity{}
	}

	affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution =
		append(affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution, *podAffinityTerm)
}

func podAffinityTermUsingAffinityAssistant(affinityAssistantName string) *corev1.PodAffinityTerm {
	return &corev1.PodAffinityTerm{LabelSelector: &metav1.LabelSelector{
		MatchLabels: map[string]string{
			workspace.LabelInstance: affinityAssistantName,
			//workspace.LabelComponent: workspace.ComponentNameAffinityAssistant,
		},
	},
		TopologyKey: "kubernetes.io/hostname",
	}
}

func getPodAffinityValue(pipelineRunName string) string {
	hashBytes := sha256.Sum256([]byte(pipelineRunName))
	hashString := fmt.Sprintf("%x", hashBytes)
	return fmt.Sprintf("%s-%s", "custom-pod-affinity", hashString[:10])
}