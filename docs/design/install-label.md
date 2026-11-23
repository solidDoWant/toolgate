# Install label design

Several toolgate installs may share a cluster. Each one manages only the objects it created.

- **Install ID.** The table `install` has one row with a random 32-character hex ID. The gateway creates it on first start (`store.InstallID`). It lives in the database, so it survives restarts, release renames and leader changes. Replicas of one install share it.
- **Label.** Every object the runtime creates carries `toolgate.soliddowant.dev/install: <ID>`: instance namespaces, the objects rendered for an instance, claims, the anchor in the shared namespace, and the PVs the gateway labels.
- **Sweep.** It deletes an orphaned namespace or shared-namespace object only if the object has its own install label, or has none and a live or stopping instance row of the install names it. An object with another install's label is never read as an orphan. An unlabeled object with no matching row is never deleted. It is logged once as unowned.
- **Adoption.** Before each pass, the sweep adds the label to unlabeled shared-namespace objects whose instance row exists. It does not patch namespaces: the gateway has no right to change a namespace, so an unlabeled namespace with a matching row counts as the install's own without the label.
- **Volume clean-up.** It refuses a PV that carries another install's label, like a PV without toolgate's labels.
- **Teardown** acts only on instance rows of its own database, whose IDs are random, so it needs no extra check.
