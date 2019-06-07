package govmomi

import (
	"errors"
	"fmt"
	"io/ioutil"
	"time"

	corev1 "k8s.io/api/core/v1"
	machineryerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	tokenapi "k8s.io/cluster-bootstrap/token/api"
	tokenutil "k8s.io/cluster-bootstrap/token/util"
	"k8s.io/klog"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/cloud/vsphere/constants"
	vsphereutils "sigs.k8s.io/cluster-api-provider-vsphere/pkg/cloud/vsphere/utils"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	apierrors "sigs.k8s.io/cluster-api/pkg/errors"
)

var (
	DefaultSSHPublicKeyFile = "/root/.ssh/vsphere_tmp.pub"
)

func (pv *Provisioner) GetKubeadmToken(cluster *clusterv1.Cluster) (string, error) {
	var token string
	if cluster.ObjectMeta.Annotations != nil {
		if token, ok := cluster.ObjectMeta.Annotations[constants.KubeadmToken]; ok {
			tokenexpirytime, err := time.Parse(time.RFC3339, cluster.ObjectMeta.Annotations[constants.KubeadmTokenExpiryTime])
			if err == nil && tokenexpirytime.Sub(time.Now()) > constants.KubeadmTokenLeftTime {
				return token, nil
			}
		}
	}
	// From the cluster locate the control plane node
	controlPlaneMachines, err := vsphereutils.GetControlPlaneMachinesForCluster(cluster, pv.lister)
	if err != nil {
		return "", err
	}

	if len(controlPlaneMachines) == 0 {
		return "", errors.New("No control plane nodes available")
	}

	kubeconfig, err := pv.GetKubeConfig(cluster)
	if err != nil {
		return "", err
	}

	token, err = pv.createKubeadmToken(kubeconfig)
	if err != nil {
		return "", err
	}

	ncluster := cluster.DeepCopy()
	if ncluster.ObjectMeta.Annotations == nil {
		ncluster.ObjectMeta.Annotations = make(map[string]string)
	}
	ncluster.ObjectMeta.Annotations[constants.KubeadmToken] = token
	// Even though this time might be off by few sec compared to the actual expiry on the token it should not have any impact
	ncluster.ObjectMeta.Annotations[constants.KubeadmTokenExpiryTime] = time.Now().Add(constants.KubeadmTokenTtl).Format(time.RFC3339)
	_, err = pv.clusterV1alpha1.Clusters(cluster.Namespace).Update(ncluster)
	if err != nil {
		klog.Infof("Could not cache the kubeadm token on cluster object: %s", err)
	}
	return token, err
}

// The logic for this function comes from various places in kubeadm itself
func (pv *Provisioner) createKubeadmToken(kubeconfig string) (string, error) {
	// Create the actual token string
	token, err := tokenutil.GenerateBootstrapToken()
	if err != nil || token == "" {
		return "", fmt.Errorf("unable to create kubeadm token: %s", err)
	}

	// Create a secret to write back to the cluster
	tokenID := token[0:tokenapi.BootstrapTokenIDBytes]
	tokenSecret := token[tokenapi.BootstrapTokenIDBytes+1:]
	expiration := time.Now().UTC().Add(constants.KubeadmTokenTtl).Format(time.RFC3339)
	data := map[string][]byte{
		tokenapi.BootstrapTokenIDKey:               []byte(tokenID),
		tokenapi.BootstrapTokenSecretKey:           []byte(tokenSecret),
		tokenapi.BootstrapTokenExpirationKey:       []byte(expiration),
		tokenapi.BootstrapTokenUsageAuthentication: []byte("true"),
		tokenapi.BootstrapTokenUsageSigningKey:     []byte("true"),
		tokenapi.BootstrapTokenExtraGroupsKey:      []byte("system:bootstrappers:kubeadm:default-node-token"),
		tokenapi.BootstrapTokenDescriptionKey:      []byte("bootstrap token generated by cluster-api-provider-vsphere"),
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tokenutil.BootstrapTokenSecretName(tokenID),
			Namespace: metav1.NamespaceSystem,
		},
		Type: corev1.SecretType(tokenapi.SecretTypeBootstrapToken),
		Data: data,
	}

	// Create a client to the target cluster
	rc, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfig))
	if err != nil {
		return "", err
	}

	clusterclient, err := kubernetes.NewForConfig(rest.AddUserAgent(rc, "cluster-api-provider-vsphere"))
	if err != nil {
		return "", err
	}

	// Create or update the secret.  Just repeating what kubeadm is doing.
	if _, err := clusterclient.CoreV1().Secrets(metav1.NamespaceSystem).Create(secret); err != nil {
		if !machineryerrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("unable to create bootstrap token secret: %s", err.Error())
		}

		if _, err := clusterclient.CoreV1().Secrets(metav1.NamespaceSystem).Update(secret); err != nil {
			return "", fmt.Errorf("unable to update bootstrap token secret: %s", err.Error())
		}
	}

	return token, nil
}

// If the Provisioner has a client for updating Machine objects, this will set
// the appropriate reason/message on the Machine.Status. If not, such as during
// cluster installation, it will operate as a no-op. It also returns the
// original error for convenience, so callers can do "return handleMachineError(...)".
func (pv *Provisioner) HandleMachineError(machine *clusterv1.Machine, err *apierrors.MachineError, eventAction string) error {
	if pv.clusterV1alpha1 != nil {
		nmachine := machine.DeepCopy()
		reason := err.Reason
		message := err.Message
		nmachine.Status.ErrorReason = &reason
		nmachine.Status.ErrorMessage = &message
		pv.clusterV1alpha1.Machines(nmachine.Namespace).UpdateStatus(nmachine)
	}
	if eventAction != "" {
		pv.eventRecorder.Eventf(machine, corev1.EventTypeWarning, "Failed"+eventAction, "%v", err.Reason)
	}

	klog.Errorf("Machine error: %v", err.Message)
	return err
}

// If the Provisioner has a client for updating Cluster objects, this will set
// the appropriate reason/message on the Cluster.Status. If not, such as during
// cluster installation, it will operate as a no-op. It also returns the
// original error for convenience, so callers can do "return handleClusterError(...)".
func (pv *Provisioner) HandleClusterError(cluster *clusterv1.Cluster, err *apierrors.ClusterError, eventAction string) error {
	if pv.clusterV1alpha1 != nil {
		ncluster := cluster.DeepCopy()
		reason := err.Reason
		message := err.Message
		ncluster.Status.ErrorReason = reason
		ncluster.Status.ErrorMessage = message
		pv.clusterV1alpha1.Clusters(ncluster.Namespace).UpdateStatus(ncluster)
	}
	if eventAction != "" {
		pv.eventRecorder.Eventf(cluster, corev1.EventTypeWarning, "Failed"+eventAction, "%v", err.Reason)
	}

	klog.Errorf("Cluster error: %v", err.Message)
	return err
}

func (pv *Provisioner) GetSSHPublicKey(cluster *clusterv1.Cluster) (string, error) {
	// First try to read the public key file from the mounted secrets volume
	key, err := ioutil.ReadFile(DefaultSSHPublicKeyFile)
	if err == nil {
		return string(key), nil
	}

	// If the mounted secrets volume not found, try to request it from the API server.
	// TODO(sflxn): We're trying to pull secrets from the default namespace and with name 'sshkeys'.  With
	// the CRD changes, this is no longer the case.  These two values are generated from kustomize.  We
	// need a different way to pass knowledge of the namespace and sshkeys into this container.
	secret, err := pv.k8sClient.Core().Secrets(cluster.Namespace).Get("sshkeys", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return string(secret.Data["vsphere_tmp.pub"]), nil
}

func (pv *Provisioner) GetKubeConfig(cluster *clusterv1.Cluster) (string, error) {
	secret, err := pv.k8sClient.Core().Secrets(cluster.Namespace).Get(fmt.Sprintf(constants.KubeConfigSecretName, cluster.UID), metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return string(secret.Data[constants.KubeConfigSecretData]), nil
}

func (pv *Provisioner) GetVsphereCredentials(cluster *clusterv1.Cluster) (string, string, error) {
	vsphereConfig, err := vsphereutils.GetClusterProviderSpec(cluster.Spec.ProviderSpec)
	if err != nil {
		return "", "", err
	}
	// If the vsphereCredentialSecret is specified then read that secret to get the credentials
	if vsphereConfig.VsphereCredentialSecret != "" {
		klog.V(4).Infof("Fetching vsphere credentials from secret %s", vsphereConfig.VsphereCredentialSecret)
		secret, err := pv.k8sClient.Core().Secrets(cluster.Namespace).Get(vsphereConfig.VsphereCredentialSecret, metav1.GetOptions{})
		if err != nil {
			klog.Warningf("Error reading secret %s", vsphereConfig.VsphereCredentialSecret)
			return "", "", err
		}
		if username, ok := secret.Data[constants.VsphereUserKey]; ok {
			if password, ok := secret.Data[constants.VspherePasswordKey]; ok {
				return string(username), string(password), nil
			}
		}
		return "", "", fmt.Errorf("Improper secret: Secret %s should have the keys `%s` and `%s` defined in it", vsphereConfig.VsphereCredentialSecret, constants.VsphereUserKey, constants.VspherePasswordKey)
	}
	return vsphereConfig.VsphereUser, vsphereConfig.VspherePassword, nil

}
