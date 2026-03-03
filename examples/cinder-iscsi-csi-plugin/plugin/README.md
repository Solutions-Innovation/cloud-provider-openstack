# Deploy the cinder-iscsi CSI plugin for dev testing
#
# This folder contains all manifests needed to install the plugin.
# Apply in order with:
#
#   kubectl apply -f examples/cinder-iscsi-csi-plugin/plugin/
#
# Or step by step:
#   kubectl apply -f secret.yaml
#   kubectl apply -f csi-driver.yaml
#   kubectl apply -f rbac.yaml
#   kubectl apply -f controller.yaml
#   kubectl apply -f node.yaml
#
# Teardown:
#   kubectl delete -f examples/cinder-iscsi-csi-plugin/plugin/
