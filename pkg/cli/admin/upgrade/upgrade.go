package upgrade

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/blang/semver"
	"github.com/spf13/cobra"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	kcmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/templates"

	configv1 "github.com/openshift/api/config/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned"
	imagereference "github.com/openshift/library-go/pkg/image/reference"

	"github.com/openshift/oc/pkg/cli/admin/upgrade/channel"
)

var upgradeExample = templates.Examples(`
	# Review the available cluster updates
	oc adm upgrade

	# Update to the latest version
	oc adm upgrade --to-latest=true
`)

func NewOptions(streams genericclioptions.IOStreams) *Options {
	return &Options{
		IOStreams: streams,
	}
}

func New(f kcmdutil.Factory, streams genericclioptions.IOStreams) *cobra.Command {
	o := NewOptions(streams)
	cmd := &cobra.Command{
		Use:     "upgrade --to=VERSION",
		Short:   "Upgrade a cluster",
		Example: upgradeExample,
		Long: templates.LongDesc(`
			Check on upgrade status or upgrade the cluster to a newer version

			This command assists with cluster upgrades. If no arguments are passed
			the command will retrieve the current version info and display whether an upgrade is
			in progress or whether any errors might prevent an upgrade, as well as show the suggested
			updates available to the cluster. Information about compatible updates is periodically
			retrieved from the update server and cached on the cluster - these are updates that are
			known to be supported as upgrades from the current version.

			Passing --to=VERSION will upgrade the cluster to one of the available updates or report
			an error if no such version exists. The cluster will then upgrade itself and report
			status that is available via "oc get clusterversion" and "oc describe clusterversion".

			If the desired upgrade from --to-image is not in the list of available versions, you must
			pass --allow-explicit-upgrade to allow upgrade to proceed. If the cluster is
			already being upgraded, or if the cluster is reporting a failure or other error, you
			must pass --allow-upgrade-with-warnings to proceed (see note below on the implications).

			If the cluster reports that the upgrade should not be performed due to a content
			verification error or update precondition failures such as operators blocking upgrades.
			Do not upgrade to images that are not appropriately signed without understanding the risks
			of upgrading your cluster to untrusted code. If you must override this protection use
			the --force flag.

			If there are no versions available, or a bug in the cluster version operator prevents
			updates from being retrieved, the more powerful and dangerous --to-image=IMAGE option
			may be used. This instructs the cluster to upgrade to the contents of the specified release
			image, regardless of whether that upgrade is safe to apply to the current version. While
			rolling back to a previous micro version (4.1.2 -> 4.1.1) may be safe, upgrading more
			than one minor version ahead (4.1 -> 4.3) or downgrading one minor version (4.2 -> 4.1)
			is likely to cause data corruption or to completely break a cluster. Avoid upgrading
			when the cluster is reporting errors or when another upgrade is in progress unless the
			upgrade cannot make progress.
		`),
		Run: func(cmd *cobra.Command, args []string) {
			kcmdutil.CheckErr(o.Complete(f, cmd, args))
			kcmdutil.CheckErr(o.Run())
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&o.To, "to", o.To, "Specify the version to upgrade to. The version must be on the list of available updates.")
	flags.StringVar(&o.ToImage, "to-image", o.ToImage, "Provide a release image to upgrade to. WARNING: This option does not check for upgrade compatibility and may break your cluster.")
	flags.BoolVar(&o.ToLatestAvailable, "to-latest", o.ToLatestAvailable, "Use the next available version")
	flags.BoolVar(&o.Clear, "clear", o.Clear, "If an upgrade has been requested but not yet downloaded, cancel the update. This has no effect once the update has started.")
	flags.BoolVar(&o.Force, "force", o.Force, "Forcefully upgrade the cluster even when upgrade release image validation fails and the cluster is reporting errors.")
	flags.BoolVar(&o.AllowExplicitUpgrade, "allow-explicit-upgrade", o.AllowExplicitUpgrade, "Upgrade even if the upgrade target is not listed in the available versions list.")
	flags.BoolVar(&o.AllowUpgradeWithWarnings, "allow-upgrade-with-warnings", o.AllowUpgradeWithWarnings, "Upgrade even if an upgrade is in process or a cluster error is blocking the update.")
	flags.BoolVar(&o.IncludeNotRecommended, "include-not-recommended", o.IncludeNotRecommended, "Display additional updates which are not recommended based on your cluster configuration.")
	flags.BoolVar(&o.AllowNotRecommended, "allow-not-recommended", o.AllowNotRecommended, "Allows upgrade to a version when it is supported but not recommended for updates")

	cmd.AddCommand(channel.New(f, streams))

	return cmd
}

type Options struct {
	genericclioptions.IOStreams

	To                string
	ToImage           string
	ToLatestAvailable bool

	AllowExplicitUpgrade     bool
	AllowUpgradeWithWarnings bool
	Force                    bool
	Clear                    bool
	IncludeNotRecommended    bool
	AllowNotRecommended      bool

	Client configv1client.Interface
}

func (o *Options) Complete(f kcmdutil.Factory, cmd *cobra.Command, args []string) error {
	if o.Clear && (len(o.ToImage) > 0 || len(o.To) > 0 || o.ToLatestAvailable) {
		return fmt.Errorf("--clear may not be specified with any other flags")
	}
	if len(o.To) > 0 && len(o.ToImage) > 0 {
		return fmt.Errorf("only one of --to or --to-image may be provided")
	}

	if len(o.To) > 0 {
		if _, err := semver.Parse(o.To); err != nil {
			return fmt.Errorf("--to must be a semantic version (e.g. 4.0.1 or 4.1.0-nightly-20181104): %v", err)
		}
	}
	// defend against simple mistakes (4.0.1 is a valid container image)
	if len(o.ToImage) > 0 {
		ref, err := imagereference.Parse(o.ToImage)
		if err != nil {
			return fmt.Errorf("--to-image must be a valid image pull spec: %v", err)
		}
		if len(ref.Registry) == 0 && len(ref.Namespace) == 0 {
			return fmt.Errorf("--to-image must be a valid image pull spec: no registry or repository specified")
		}
		if len(ref.ID) == 0 && len(ref.Tag) == 0 {
			return fmt.Errorf("--to-image must be a valid image pull spec: no tag or digest specified")
		}
		if len(ref.Tag) > 0 {
			if o.Force {
				fmt.Fprintln(o.ErrOut, "warning: Using by-tag pull specs is dangerous, and while we still allow it in combination with --force for backward compatibility, it would be much safer to pass a by-digest pull spec instead")
			} else {
				return fmt.Errorf("--to-image must be a by-digest pull spec, unless --force is also set, because release images that are not accessed via digest cannot be verified by the cluster.  Even when --force is set, using tags is not recommended, although we continue to allow it for backwards compatibility")
			}
		}
	}

	cfg, err := f.ToRESTConfig()
	if err != nil {
		return err
	}
	client, err := configv1client.NewForConfig(cfg)
	if err != nil {
		return err
	}
	o.Client = client
	return nil
}

func (o *Options) Run() error {
	cv, err := o.Client.ConfigV1().ClusterVersions().Get(context.TODO(), "version", metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("No cluster version information available - you must be connected to an OpenShift version 4 server to fetch the current version")
		}
		return err
	}

	switch {
	case o.Clear:
		if cv.Spec.DesiredUpdate == nil {
			fmt.Fprintf(o.Out, "info: No update in progress\n")
			return nil
		}
		original := cv.Spec.DesiredUpdate
		cv.Spec.DesiredUpdate = nil
		updated, err := o.Client.ConfigV1().ClusterVersions().Patch(context.TODO(), cv.Name, types.MergePatchType, []byte(`{"spec":{"desiredUpdate":null}}`), metav1.PatchOptions{})
		if err != nil {
			return fmt.Errorf("Unable to cancel current rollout: %v", err)
		}
		if updateIsEquivalent(*original, updated.Status.Desired) {
			fmt.Fprintf(o.Out, "Cleared the update field, still at %s\n", releaseVersionString(updated.Status.Desired))
		} else {
			fmt.Fprintf(o.Out, "Cancelled requested upgrade to %s\n", updateVersionString(*original))
		}
		return nil

	case o.ToLatestAvailable:
		if len(cv.Status.AvailableUpdates) == 0 {
			fmt.Fprintf(o.Out, "info: Cluster is already at the latest available version %s\n", cv.Status.Desired.Version)
			return nil
		}

		if err := checkForUpgrade(cv); err != nil {
			if !o.AllowUpgradeWithWarnings {
				return fmt.Errorf("%s\n\nIf you want to upgrade anyway, use --allow-upgrade-with-warnings.", err)
			}
			fmt.Fprintf(o.ErrOut, "warning: --allow-upgrade-with-warnings is bypassing: %s", err)
		}

		sortReleasesBySemanticVersions(cv.Status.AvailableUpdates)

		update := cv.Status.AvailableUpdates[0]
		desiredUpdate := &configv1.Update{
			Version: update.Version,
			Image:   update.Image,
		}
		if o.Force {
			desiredUpdate.Force = true
			fmt.Fprintln(o.ErrOut, "warning: --force overrides cluster verification of your supplied release image and waives any update precondition failures.")
		}

		cv.Spec.DesiredUpdate = desiredUpdate
		_, err := o.Client.ConfigV1().ClusterVersions().Update(context.TODO(), cv, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("Unable to upgrade to latest version %s: %v", update.Version, err)
		}

		if len(update.Version) > 0 {
			fmt.Fprintf(o.Out, "Updating to latest version %s\n", update.Version)
		} else {
			fmt.Fprintf(o.Out, "Updating to latest release image %s\n", update.Image)
		}

		return nil

	case len(o.To) > 0, len(o.ToImage) > 0:
		var update *configv1.Update
		if len(o.To) > 0 && o.To == cv.Status.Desired.Version {
			fmt.Fprintf(o.Out, "info: Cluster is already at version %s\n", o.To)
			return nil
		}

		if len(o.ToImage) > 0 && o.ToImage == cv.Status.Desired.Image {
			fmt.Fprintf(o.Out, "info: Cluster is already at %s\n", o.ToImage)
			return nil
		}

		possibleUpgradeTargets := make([]string, 0, len(cv.Status.AvailableUpdates)+len(cv.Status.ConditionalUpdates))

		// check for recommended updates
		for _, available := range cv.Status.AvailableUpdates {
			if match, err := targetMatch(&available, o.To, o.ToImage); match && err == nil {
				update = &configv1.Update{
					Version: available.Version,
					Image:   available.Image,
				}
				break
			} else if err != nil {
				fmt.Fprintf(o.ErrOut, "warning: unable to calculate match for the update target in available updates: %v\n", err)
			}
			possibleUpgradeTargets = append(possibleUpgradeTargets, available.Version)
		}

		if update == nil {
			// update was not recommended, so check for conditional, but not recommended, updates
			for _, upgrade := range cv.Status.ConditionalUpdates {
				if c := findCondition(upgrade.Conditions, "Recommended"); c != nil && c.Status != metav1.ConditionTrue {
					if match, err := targetMatch(&upgrade.Release, o.To, o.ToImage); match && err == nil {
						if !o.AllowNotRecommended {
							return fmt.Errorf("the update %s is not one of the recommended updates, but is available as a conditional update."+
								"To accept the %s=%s risk and to proceed with update use --allow-not-recommended.\n  Reason: %s\n  Message: %s\n",
								upgrade.Release.Version, c.Type, c.Status, c.Reason, strings.ReplaceAll(c.Message, "\n", "\n  "))
						}
						update = &configv1.Update{
							Version: upgrade.Release.Version,
							Image:   upgrade.Release.Image,
						}
						fmt.Fprintf(o.ErrOut, "warning: with --allow-not-recommended you have accepted the risks with %s and bypassed %s=%s %s: %s\n",
							upgrade.Release.Version, c.Type, c.Status, c.Reason, c.Message)
						break
					} else if err != nil {
						fmt.Fprintf(o.ErrOut, "warning: unable to calculate match for the update target in available conditional updates: %v\n", err)
					}
					if o.AllowNotRecommended {
						possibleUpgradeTargets = append(possibleUpgradeTargets, upgrade.Release.Version)
					}
				}
			}
		}

		if o.ToImage != "" {
			if o.AllowExplicitUpgrade {
				update = &configv1.Update{
					Version: "",
					Image:   o.ToImage,
				}
				fmt.Fprintln(o.ErrOut, "warning: The requested upgrade image is not one of the available updates."+
					"You have used --allow-explicit-upgrade for the update to proceed anyway")
			}
		}

		if update == nil {
			sort.Strings(possibleUpgradeTargets)
			c := findClusterOperatorStatusCondition(cv.Status.Conditions, configv1.RetrievedUpdates)

			toImageOption := "--to-image"
			if o.ToImage == "" {
				toImageOption = "--allow-explicit-upgrade"
			}
			nextStep := fmt.Sprintf("%s to continue with the update", toImageOption)

			switch {
			case c != nil && c.Status != configv1.ConditionTrue:
				return fmt.Errorf("cannot refresh available updates:\n  Reason: %s\n  Message: %s\n\nspecify %s.", c.Reason, strings.ReplaceAll(c.Message, "\n", "\n  "), nextStep)
			case len(possibleUpgradeTargets) == 0 && o.AllowNotRecommended:
				return fmt.Errorf("no recommended or conditional updates, specify %s or wait for new updates to be available.", nextStep)
			case len(possibleUpgradeTargets) == 0:
				return fmt.Errorf("no recommended updates, specify %s or wait for new updates to be available.", nextStep)
			case len(possibleUpgradeTargets) > 0:
				return fmt.Errorf("the update is not one of the possible targets: %s. specify %s.", strings.Join(possibleUpgradeTargets, ", "), nextStep)
			default:
				return errors.New("unable to calculate a target update")
			}
		}

		if o.Force {
			update.Force = true
			fmt.Fprintln(o.ErrOut, "warning: --force overrides cluster verification of your supplied release image and waives any update precondition failures.")
		}

		if err := checkForUpgrade(cv); err != nil {
			if !o.AllowUpgradeWithWarnings {
				return fmt.Errorf("%s\n\nIf you want to upgrade anyway, use --allow-upgrade-with-warnings.", err)
			}
			fmt.Fprintf(o.ErrOut, "warning: --allow-upgrade-with-warnings is bypassing: %s", err)
		}

		cv.Spec.DesiredUpdate = update

		_, err := o.Client.ConfigV1().ClusterVersions().Update(context.TODO(), cv, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("Unable to upgrade: %v", err)
		}

		if len(update.Version) > 0 {
			fmt.Fprintf(o.Out, "Updating to %s\n", update.Version)
		} else {
			fmt.Fprintf(o.Out, "Updating to release image %s\n", update.Image)
		}

		return nil

	default:
		if c := findClusterOperatorStatusCondition(cv.Status.Conditions, configv1.OperatorDegraded); c != nil && c.Status == configv1.ConditionTrue {
			prefix := "No upgrade is possible due to an error"
			if c := findClusterOperatorStatusCondition(cv.Status.Conditions, configv1.OperatorProgressing); c != nil && c.Status == configv1.ConditionTrue && len(c.Message) > 0 {
				prefix = c.Message
			}
			if len(c.Message) > 0 {
				return fmt.Errorf("%s:\n\n  Reason: %s\n  Message: %s\n\n", prefix, c.Reason, strings.ReplaceAll(c.Message, "\n", "\n  "))
			}
			return fmt.Errorf("The cluster can't be upgraded, see `oc describe clusterversion`")
		}

		if c := findClusterOperatorStatusCondition(cv.Status.Conditions, configv1.OperatorProgressing); c != nil && len(c.Message) > 0 {
			if c.Status == configv1.ConditionTrue {
				fmt.Fprintf(o.Out, "info: An upgrade is in progress. %s\n", c.Message)
			} else {
				fmt.Fprintln(o.Out, c.Message)
			}
		} else {
			fmt.Fprintln(o.ErrOut, "warning: No current status info, see `oc describe clusterversion` for more details")
		}
		fmt.Fprintln(o.Out)

		if c := findClusterOperatorStatusCondition(cv.Status.Conditions, configv1.OperatorUpgradeable); c != nil && c.Status == configv1.ConditionFalse {
			fmt.Fprintf(o.Out, "Upgradeable=False\n\n  Reason: %s\n  Message: %s\n\n", c.Reason, strings.ReplaceAll(c.Message, "\n", "\n  "))
		}

		if cv.Spec.Channel != "" {
			if cv.Spec.Upstream == "" {
				fmt.Fprint(o.Out, "Upstream is unset, so the cluster will use an appropriate default.\n")
			} else {
				fmt.Fprintf(o.Out, "Upstream: %s\n", cv.Spec.Upstream)
			}
			if len(cv.Status.Desired.Channels) > 0 {
				fmt.Fprintf(o.Out, "Channel: %s (available channels: %s)\n", cv.Spec.Channel, strings.Join(cv.Status.Desired.Channels, ", "))
			} else {
				fmt.Fprintf(o.Out, "Channel: %s\n", cv.Spec.Channel)
			}
		}

		if len(cv.Status.AvailableUpdates) > 0 {
			fmt.Fprintf(o.Out, "\nRecommended updates:\n\n")
			// set the minimal cell width to 14 to have a larger space between the columns for shorter versions
			w := tabwriter.NewWriter(o.Out, 14, 2, 1, ' ', 0)
			fmt.Fprintf(w, "  VERSION\tIMAGE\n")
			// TODO: add metadata about version
			sortReleasesBySemanticVersions(cv.Status.AvailableUpdates)
			for _, update := range cv.Status.AvailableUpdates {
				fmt.Fprintf(w, "  %s\t%s\n", update.Version, update.Image)
			}
			w.Flush()
			if c := findClusterOperatorStatusCondition(cv.Status.Conditions, configv1.RetrievedUpdates); c != nil && c.Status == configv1.ConditionFalse {
				fmt.Fprintf(o.ErrOut, "warning: Cannot refresh available updates:\n  Reason: %s\n  Message: %s\n\n", c.Reason, strings.ReplaceAll(c.Message, "\n", "\n  "))
			}
		} else {
			if c := findClusterOperatorStatusCondition(cv.Status.Conditions, configv1.RetrievedUpdates); c != nil && c.Status == configv1.ConditionFalse {
				fmt.Fprintf(o.ErrOut, "warning: Cannot display available updates:\n  Reason: %s\n  Message: %s\n\n", c.Reason, strings.ReplaceAll(c.Message, "\n", "\n  "))
			} else {
				fmt.Fprintf(o.Out, "No updates available. You may force an upgrade to a specific release image, but doing so may not be supported and may result in downtime or data loss.\n")
			}
		}

		if o.IncludeNotRecommended {
			if containsNotRecommendedUpdate(cv.Status.ConditionalUpdates) {
				sortConditionalUpdatesBySemanticVersions(cv.Status.ConditionalUpdates)
				fmt.Fprintf(o.Out, "\nSupported but not recommended updates:\n")
				for _, update := range cv.Status.ConditionalUpdates {
					if c := findCondition(update.Conditions, "Recommended"); c != nil && c.Status != metav1.ConditionTrue {
						fmt.Fprintf(o.Out, "\n  Version: %s\n  Image: %s\n", update.Release.Version, update.Release.Image)
						fmt.Fprintf(o.Out, "  Recommended: %s\n  Reason: %s\n  Message: %s\n", c.Status, c.Reason, strings.ReplaceAll(strings.TrimSpace(c.Message), "\n", "\n  "))
					}
				}
			} else {
				fmt.Fprintf(o.Out, "\nNo updates which are not recommended based on your cluster configuration are available.\n")
			}
		} else if containsNotRecommendedUpdate(cv.Status.ConditionalUpdates) {
			fmt.Fprintf(o.Out, "\nAdditional updates which are not recommended based on your cluster configuration are available, to view those re-run the command with --include-not-recommended.\n")
		}

		// TODO: print previous versions
	}

	return nil
}

func containsNotRecommendedUpdate(updates []configv1.ConditionalUpdate) bool {
	for _, update := range updates {
		if c := findCondition(update.Conditions, "Recommended"); c != nil && c.Status != metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func errorList(errs []error) string {
	if len(errs) == 1 {
		return errs[0].Error()
	}
	buf := &bytes.Buffer{}
	fmt.Fprintf(buf, "\n\n")
	for _, err := range errs {
		fmt.Fprintf(buf, "* %v\n", err)
	}
	return buf.String()
}

func updateVersionString(update configv1.Update) string {
	if len(update.Version) > 0 {
		return update.Version
	}
	if len(update.Image) > 0 {
		return update.Image
	}
	return "<unknown>"
}

func releaseVersionString(release configv1.Release) string {
	if len(release.Version) > 0 {
		return release.Version
	}
	if len(release.Image) > 0 {
		return release.Image
	}
	return "<unknown>"
}

func stringArrContains(arr []string, s string) bool {
	for _, item := range arr {
		if item == s {
			return true
		}
	}
	return false
}

func writeTabSection(out io.Writer, fn func(w io.Writer)) {
	w := tabwriter.NewWriter(out, 0, 4, 1, ' ', 0)
	fn(w)
	w.Flush()
}

func updateIsEquivalent(a configv1.Update, b configv1.Release) bool {
	switch {
	case len(a.Image) > 0 && len(b.Image) > 0:
		return a.Image == b.Image
	case len(a.Version) > 0 && len(b.Version) > 0:
		return a.Version == b.Version
	default:
		return false
	}
}

// sortReleasesBySemanticVersions sorts the input slice in decreasing order.
func sortReleasesBySemanticVersions(versions []configv1.Release) {
	sort.Slice(versions, func(i, j int) bool {
		a, errA := semver.Parse(versions[i].Version)
		b, errB := semver.Parse(versions[j].Version)
		if errA == nil && errB != nil {
			return true
		}
		if errB == nil && errA != nil {
			return false
		}
		if errA != nil && errB != nil {
			return versions[i].Version > versions[j].Version
		}
		return a.GT(b)
	})
}

// sortConditionalUpdatesBySemanticVersions sorts the input slice in decreasing order.
func sortConditionalUpdatesBySemanticVersions(updates []configv1.ConditionalUpdate) {
	sort.Slice(updates, func(i, j int) bool {
		a, errA := semver.Parse(updates[i].Release.Version)
		b, errB := semver.Parse(updates[j].Release.Version)
		if errA == nil && errB != nil {
			return true
		}
		if errB == nil && errA != nil {
			return false
		}
		if errA != nil && errB != nil {
			return updates[i].Release.Version > updates[j].Release.Version
		}
		return a.GT(b)
	})
}

func versionStrings(updates []configv1.Release) []string {
	var arr []string
	for _, update := range updates {
		arr = append(arr, update.Version)
	}
	return arr
}

func findCondition(conditions []metav1.Condition, name string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == name {
			return &conditions[i]
		}
	}
	return nil
}

func findClusterOperatorStatusCondition(conditions []configv1.ClusterOperatorStatusCondition, name configv1.ClusterStatusConditionType) *configv1.ClusterOperatorStatusCondition {
	for i := range conditions {
		if conditions[i].Type == name {
			return &conditions[i]
		}
	}
	return nil
}

func checkForUpgrade(cv *configv1.ClusterVersion) error {
	results := []string{}
	if c := findClusterOperatorStatusCondition(cv.Status.Conditions, "Invalid"); c != nil && c.Status == configv1.ConditionTrue {
		results = append(results, fmt.Sprintf("the cluster version object is invalid, you must correct the invalid state first:\n\n  Reason: %s\n  Message: %s\n\n", c.Reason, strings.ReplaceAll(c.Message, "\n", "\n  ")))
	}
	if c := findClusterOperatorStatusCondition(cv.Status.Conditions, configv1.OperatorDegraded); c != nil && c.Status == configv1.ConditionTrue {
		results = append(results, fmt.Sprintf("the cluster is experiencing an upgrade-blocking error:\n\n  Reason: %s\n  Message: %s\n\n", c.Reason, strings.ReplaceAll(c.Message, "\n", "\n  ")))
	}
	if c := findClusterOperatorStatusCondition(cv.Status.Conditions, configv1.OperatorProgressing); c != nil && c.Status == configv1.ConditionTrue {
		results = append(results, fmt.Sprintf("the cluster is already upgrading:\n\n  Reason: %s\n  Message: %s\n\n", c.Reason, strings.ReplaceAll(c.Message, "\n", "\n  ")))
	}

	if len(results) == 0 {
		return nil
	}

	return errors.New(strings.Join(results, ""))
}

// targetMatch returns true if the target release matches the target
// 'to' version string or 'toImage' pullspec.  Empty 'to' or 'toImage'
// strings will not match, even in the unlikely event that the version
// and image strings in the 'target' are also empty.
func targetMatch(target *configv1.Release, to string, toImage string) (bool, error) {
	if to != "" && target.Version == to {
		return true, nil
	}

	if toImage != "" {
		// if images exactly match
		if target.Image == toImage {
			return true, nil
		}

		// if digests match (signature verification would match)
		if refTarget, err := imagereference.Parse(target.Image); err != nil {
			return false, err
		} else {
			if refTo, err := imagereference.Parse(toImage); err != nil {
				return false, err
			} else if len(refTo.ID) > 0 && refTarget.ID == refTo.ID {
				return true, nil
			}
		}
	}

	return false, nil
}
