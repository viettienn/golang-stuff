package packer

import (
	"fmt"
	"log"
	"sync"
)

// This is the key in configurations that is set to "true" when Packer
// debugging is enabled.
const DebugConfigKey = "packer_debug"

// A Build represents a single job within Packer that is responsible for
// building some machine image artifact. Builds are meant to be parallelized.
type Build interface {
	// Name is the name of the build. This is unique across a single template,
	// but not absolutely unique. This is meant more to describe to the user
	// what is being built rather than being a unique identifier.
	Name() string

	// Prepare configures the various components of this build and reports
	// any errors in doing so (such as syntax errors, validation errors, etc.)
	Prepare() error

	// Run runs the actual builder, returning an artifact implementation
	// of what is built. If anything goes wrong, an error is returned.
	Run(Ui, Cache) ([]Artifact, error)

	// Cancel will cancel a running build. This will block until the build
	// is actually completely cancelled.
	Cancel()

	// SetDebug will enable/disable debug mode. Debug mode is always
	// enabled by adding the additional key "packer_debug" to boolean
	// true in the configuration of the various components. This must
	// be called prior to Prepare.
	//
	// When SetDebug is set to true, parallelism between builds is
	// strictly prohibited.
	SetDebug(bool)
}

// A build struct represents a single build job, the result of which should
// be a single machine image artifact. This artifact may be comprised of
// multiple files, of course, but it should be for only a single provider
// (such as VirtualBox, EC2, etc.).
type coreBuild struct {
	name           string
	builder        Builder
	builderConfig  interface{}
	builderType    string
	hooks          map[string][]Hook
	postProcessors [][]coreBuildPostProcessor
	provisioners   []coreBuildProvisioner

	debug         bool
	l             sync.Mutex
	prepareCalled bool
}

// Keeps track of the post-processor and the configuration of the
// post-processor used within a build.
type coreBuildPostProcessor struct {
	processor         PostProcessor
	processorType     string
	config            interface{}
	keepInputArtifact bool
}

// Keeps track of the provisioner and the configuration of the provisioner
// within the build.
type coreBuildProvisioner struct {
	provisioner Provisioner
	config      []interface{}
}

// Returns the name of the build.
func (b *coreBuild) Name() string {
	return b.name
}

// Prepare prepares the build by doing some initialization for the builder
// and any hooks. This _must_ be called prior to Run.
func (b *coreBuild) Prepare() (err error) {
	b.l.Lock()
	defer b.l.Unlock()

	if b.prepareCalled {
		panic("prepare already called")
	}

	b.prepareCalled = true

	debugConfig := map[string]interface{}{
		DebugConfigKey: b.debug,
	}

	// Prepare the builder
	err = b.builder.Prepare(b.builderConfig, debugConfig)
	if err != nil {
		log.Printf("Build '%s' prepare failure: %s\n", b.name, err)
		return
	}

	// Prepare the provisioners
	for _, coreProv := range b.provisioners {
		configs := make([]interface{}, len(coreProv.config), len(coreProv.config)+1)
		copy(configs, coreProv.config)
		configs = append(configs, debugConfig)

		if err = coreProv.provisioner.Prepare(configs...); err != nil {
			return
		}
	}

	// Prepare the post-processors
	for _, ppSeq := range b.postProcessors {
		for _, corePP := range ppSeq {
			if err = corePP.processor.Configure(corePP.config); err != nil {
				return
			}
		}
	}

	return
}

// Runs the actual build. Prepare must be called prior to running this.
func (b *coreBuild) Run(originalUi Ui, cache Cache) ([]Artifact, error) {
	if !b.prepareCalled {
		panic("Prepare must be called first")
	}

	// Copy the hooks
	hooks := make(map[string][]Hook)
	for hookName, hookList := range b.hooks {
		hooks[hookName] = make([]Hook, len(hookList))
		copy(hooks[hookName], hookList)
	}

	// Add a hook for the provisioners if we have provisioners
	if len(b.provisioners) > 0 {
		provisioners := make([]Provisioner, len(b.provisioners))
		for i, p := range b.provisioners {
			provisioners[i] = p.provisioner
		}

		if _, ok := hooks[HookProvision]; !ok {
			hooks[HookProvision] = make([]Hook, 0, 1)
		}

		hooks[HookProvision] = append(hooks[HookProvision], &ProvisionHook{provisioners})
	}

	hook := &DispatchHook{hooks}
	artifacts := make([]Artifact, 0, 1)

	// The builder just has a normal Ui, but prefixed
	builderUi := &PrefixedUi{
		fmt.Sprintf("==> %s", b.Name()),
		fmt.Sprintf("    %s", b.Name()),
		originalUi,
	}

	log.Printf("Running builder: %s", b.builderType)
	builderArtifact, err := b.builder.Run(builderUi, hook, cache)
	if err != nil {
		return nil, err
	}

	// If there was no result, don't worry about running post-processors
	// because there is nothing they can do, just return.
	if builderArtifact == nil {
		return nil, nil
	}

	errors := make([]error, 0)
	keepOriginalArtifact := len(b.postProcessors) == 0

	// Run the post-processors
PostProcessorRunSeqLoop:
	for _, ppSeq := range b.postProcessors {
		priorArtifact := builderArtifact
		for i, corePP := range ppSeq {
			ppUi := &PrefixedUi{
				fmt.Sprintf("==> %s (%s)", b.Name(), corePP.processorType),
				fmt.Sprintf("    %s (%s)", b.Name(), corePP.processorType),
				originalUi,
			}

			builderUi.Say(fmt.Sprintf("Running post-processor: %s", corePP.processorType))
			artifact, err := corePP.processor.PostProcess(ppUi, priorArtifact)
			if err != nil {
				errors = append(errors, fmt.Errorf("Post-processor failed: %s", err))
				continue PostProcessorRunSeqLoop
			}

			if artifact == nil {
				log.Println("Nil artifact, halting post-processor chain.")
				continue PostProcessorRunSeqLoop
			}

			if i == 0 {
				// This is the first post-processor. We handle deleting
				// previous artifacts a bit different because multiple
				// post-processors may be using the original and need it.
				if !keepOriginalArtifact && corePP.keepInputArtifact {
					log.Printf(
						"Flagging to keep original artifact from post-processor '%s'",
						corePP.processorType)
					keepOriginalArtifact = true
				}
			} else {
				// We have a prior artifact. If we want to keep it, we append
				// it to the results list. Otherwise, we destroy it.
				if corePP.keepInputArtifact {
					artifacts = append(artifacts, priorArtifact)
				} else {
					log.Printf("Deleting prior artifact from post-processor '%s'", corePP.processorType)
					if err := priorArtifact.Destroy(); err != nil {
						errors = append(errors, fmt.Errorf("Failed cleaning up prior artifact: %s", err))
					}
				}
			}

			priorArtifact = artifact
		}

		// Add on the last artifact to the results
		if priorArtifact != nil {
			artifacts = append(artifacts, priorArtifact)
		}
	}

	if keepOriginalArtifact {
		artifacts = append(artifacts, nil)
		copy(artifacts[1:], artifacts)
		artifacts[0] = builderArtifact
	} else {
		log.Printf("Deleting original artifact for build '%s'", b.name)
		if err := builderArtifact.Destroy(); err != nil {
			errors = append(errors, fmt.Errorf("Error destroying builder artifact: %s", err))
		}
	}

	if len(errors) > 0 {
		err = &MultiError{errors}
	}

	return artifacts, err
}

func (b *coreBuild) SetDebug(val bool) {
	if b.prepareCalled {
		panic("prepare has already been called")
	}

	b.debug = val
}

// Cancels the build if it is running.
func (b *coreBuild) Cancel() {
	b.builder.Cancel()
}