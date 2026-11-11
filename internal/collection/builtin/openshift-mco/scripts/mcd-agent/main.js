var kube = require("kube");

var DESIRED = "machineconfiguration.openshift.io/desiredConfig";
var CURRENT = "machineconfiguration.openshift.io/currentConfig";
var STATE = "machineconfiguration.openshift.io/state";

exports.main = function (ctx) {
  var nodeName = ctx.pod.nodeName;
  ctx.log("mcd-agent starting on node " + nodeName);

  kube.watch(
    { group: "", version: "v1", resource: "nodes" },
    function (event) {
      var node = event.object;
      if (node.metadata.name !== nodeName) return;

      var annotations = node.metadata.annotations || {};
      var desired = annotations[DESIRED];
      var current = annotations[CURRENT];

      if (!desired || desired === current) return;

      ctx.log("config drift: desired=" + desired + " current=" + current);
      ctx.sleep(2000);

      var patch = { metadata: { annotations: {} } };
      patch.metadata.annotations[CURRENT] = desired;
      patch.metadata.annotations[STATE] = "Done";

      kube.patch(
        { group: "", version: "v1", resource: "nodes", name: nodeName },
        patch
      );

      ctx.log("config applied: " + desired);
    },
    { fieldSelector: "metadata.name=" + nodeName }
  );

  return ctx.run();
};
