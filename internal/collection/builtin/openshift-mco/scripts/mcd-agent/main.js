var kube = require("kube");

var DESIRED = "machineconfiguration.openshift.io/desiredConfig";
var CURRENT = "machineconfiguration.openshift.io/currentConfig";
var STATE = "machineconfiguration.openshift.io/state";
var BOOTSTRAP_CONFIG = "bootstrap";

var NODE_GVR = { group: "", version: "v1", resource: "nodes" };
function nodeRef(name) {
  return { group: "", version: "v1", resource: "nodes", name: name };
}

function mcnRef(name) {
  return {
    group: "machineconfiguration.openshift.io",
    version: "v1",
    resource: "machineconfignodes",
    name: name,
  };
}

function getNode(nodeName) {
  return kube.get(nodeRef(nodeName));
}

function patchNode(nodeName, annotations) {
  return kube.patch(nodeRef(nodeName), {
    metadata: { annotations: annotations },
  });
}

function ensureBootstrapState(ctx, node) {
  var annotations = node.metadata.annotations || {};
  var current = annotations[CURRENT];
  var desired = annotations[DESIRED];
  var patch = {};

  if (!current && !desired) {
    patch[CURRENT] = BOOTSTRAP_CONFIG;
    patch[DESIRED] = BOOTSTRAP_CONFIG;
    patch[STATE] = "Done";
    ctx.log("bootstrapping node annotations: " + BOOTSTRAP_CONFIG);
    return patchNode(node.metadata.name, patch);
  }

  if (!desired) {
    patch[DESIRED] = current;
  }

  if (current === desired && annotations[STATE] !== "Done") {
    patch[STATE] = "Done";
  }

  if (Object.keys(patch).length === 0) return node;
  return patchNode(node.metadata.name, patch);
}

function isUpdated(mcn, desired) {
  var status = mcn.status || {};
  var versions = status.configVersion || {};
  if (versions.current !== desired || versions.desired !== desired) return false;

  var conditions = status.conditions || [];
  for (var i = 0; i < conditions.length; i++) {
    if (conditions[i].type === "Updated" && conditions[i].status === "True") {
      return true;
    }
  }
  return false;
}

function reportMachineConfigNode(ctx, nodeName, desired) {
  var ref = mcnRef(nodeName);
  var mcn;
  try {
    mcn = kube.get(ref);
  } catch (error) {
    if (String(error).indexOf("not found") === -1) throw error;
    ctx.log("waiting for MachineConfigNode: " + nodeName);
    return;
  }

  if (isUpdated(mcn, desired)) return;

  try {
    kube.patch(
      ref,
      {
        status: {
          configVersion: { current: desired, desired: desired },
          conditions: [
            {
              type: "Updated",
              status: "True",
              reason: "Updated",
              message: "Node " + nodeName + " Updated",
              lastTransitionTime: new Date().toISOString(),
            },
          ],
        },
      },
      { subresource: "status" }
    );
  } catch (error) {
    ctx.log("failed to patch MachineConfigNode status: " + error);
  }
}

function reconcile(ctx, nodeName) {
  var node = ensureBootstrapState(ctx, getNode(nodeName));
  var annotations = node.metadata.annotations || {};
  var desired = annotations[DESIRED];
  var current = annotations[CURRENT];

  if (!desired) return;

  if (desired === current) {
    reportMachineConfigNode(ctx, nodeName, desired);
    return;
  }

  ctx.log("config drift: desired=" + desired + " current=" + current);
  ctx.sleep(2000);

  var patch = {};
  patch[CURRENT] = desired;
  patch[STATE] = "Done";
  patchNode(nodeName, patch);
  reportMachineConfigNode(ctx, nodeName, desired);

  ctx.log("config applied: " + desired);
}

exports.main = function (ctx) {
  var nodeName = ctx.pod.nodeName;
  ctx.log("mcd-agent starting on node " + nodeName);

  try {
    reconcile(ctx, nodeName);
  } catch (error) {
    ctx.log("initial reconcile failed: " + error);
  }

  kube.watch(
    NODE_GVR,
    function (event) {
      var node = event.object;
      if (node.metadata.name !== nodeName) return;

      try {
        reconcile(ctx, nodeName);
      } catch (error) {
        ctx.log("reconcile failed: " + error);
      }
    },
    { fieldSelector: "metadata.name=" + nodeName }
  );

  return ctx.run();
};
