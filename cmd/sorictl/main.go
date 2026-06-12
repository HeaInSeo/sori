package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HeaInSeo/sori"
	"github.com/HeaInSeo/sori/registryutil"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	defaultStoreName = ".sori-oci"
	helpCommand      = "help"
	helpLong         = "--help"
	outputJSON       = "json"
	outputText       = "text"
)

type pushConfig struct {
	ref             string
	sourceDir       string
	storePath       string
	registry        string
	repository      string
	tag             string
	name            string
	version         string
	displayName     string
	description     string
	kind            string
	sourceURI       string
	metadataPath    string
	username        string
	passwordEnv     string
	tokenEnv        string
	output          string
	plainHTTP       bool
	insecureTLS     bool
	caFile          string
	requireMetadata bool
	progress        bool
}

type pushOutput struct {
	Reference         string `json:"reference"`
	Repository        string `json:"repository"`
	Tag               string `json:"tag"`
	ManifestDigest    string `json:"manifestDigest"`
	LocalStore        string `json:"localStore"`
	LocalTag          string `json:"localTag"`
	TotalSize         int64  `json:"totalSize"`
	DatasetMetadata   bool   `json:"datasetMetadata"`
	MetadataMediaType string `json:"metadataMediaType,omitempty"`
}

type registryFlags struct {
	username    string
	passwordEnv string
	tokenEnv    string
	plainHTTP   bool
	insecureTLS bool
	caFile      string
	output      string
}

type imageRef struct {
	registry   string
	repository string
	tag        string
}

type inspectOutput struct {
	Reference         string                `json:"reference"`
	ManifestDigest    string                `json:"manifestDigest"`
	ConfigMediaType   string                `json:"configMediaType"`
	ArtifactFormat    string                `json:"artifactFormat,omitempty"`
	LayerCount        int                   `json:"layerCount"`
	FileCount         int                   `json:"fileCount,omitempty"`
	ChunkCount        int                   `json:"chunkCount,omitempty"`
	ChunkSize         int64                 `json:"chunkSize,omitempty"`
	TotalSize         int64                 `json:"totalSize,omitempty"`
	DatasetMetadata   bool                  `json:"datasetMetadata"`
	MetadataValid     bool                  `json:"metadataValid"`
	MetadataError     string                `json:"metadataError,omitempty"`
	MetadataMediaType string                `json:"metadataMediaType,omitempty"`
	Metadata          *sori.DatasetMetadata `json:"metadata,omitempty"`
}

type fetchOutput struct {
	Reference       string `json:"reference"`
	Destination     string `json:"destination"`
	VolumeRef       string `json:"volumeRef"`
	Skipped         bool   `json:"skipped"`
	DatasetMetadata string `json:"datasetMetadata,omitempty"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "sorictl: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return errors.New("command is required")
	}

	switch args[0] {
	case "dataset":
		return runDataset(args[1:])
	case "metadata":
		return runMetadata(args[1:])
	case helpCommand, "-h", helpLong:
		printUsage(os.Stdout)
		return nil
	default:
		printUsage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runDataset(args []string) error {
	if len(args) == 0 {
		printDatasetUsage(os.Stderr)
		return errors.New("dataset subcommand is required")
	}
	switch args[0] {
	case "push":
		return runDatasetPush(args[1:])
	case "inspect":
		return runDatasetInspect(args[1:])
	case "fetch":
		return runDatasetFetch(args[1:])
	case helpCommand, "-h", helpLong:
		printDatasetUsage(os.Stdout)
		return nil
	default:
		printDatasetUsage(os.Stderr)
		return fmt.Errorf("unknown dataset subcommand %q", args[0])
	}
}

func runMetadata(args []string) error {
	if len(args) == 0 {
		printMetadataUsage(os.Stderr)
		return errors.New("metadata subcommand is required")
	}
	switch args[0] {
	case "init":
		return runMetadataInit(args[1:])
	case helpCommand, "-h", helpLong:
		printMetadataUsage(os.Stdout)
		return nil
	default:
		printMetadataUsage(os.Stderr)
		return fmt.Errorf("unknown metadata subcommand %q", args[0])
	}
}

func runDatasetPush(args []string) error {
	cfg, err := parsePushConfig(args)
	if err != nil {
		return err
	}
	configureLibraryLogging(cfg.output)

	metadata, hasMetadata, err := loadOrBuildMetadata(cfg)
	if err != nil {
		return err
	}
	if cfg.requireMetadata && !hasMetadata {
		return errors.New("metadata is required; pass --metadata or enough flags to generate it")
	}

	client := sori.NewClient(sori.WithLocalStorePath(cfg.storePath))
	req := sori.PackageRequest{
		SourceDir:   cfg.sourceDir,
		DisplayName: firstNonEmpty(cfg.displayName, cfg.name, cfg.tag),
		Tag:         cfg.tag,
		Dataset:     cfg.name,
		Version:     cfg.version,
		Description: cfg.description,
	}
	pkg, err := client.PackageVolumeWithOptions(context.Background(), req, sori.PackageOptions{
		Format:          sori.ArtifactFormatChunkedCAS,
		DatasetMetadata: metadata,
		Progress:        progressPrinter(cfg.progress),
	})
	if err != nil {
		return fmt.Errorf("package dataset: %w", err)
	}

	push, err := client.PushPackagedVolume(context.Background(), pkg, sori.RemoteTarget{
		Registry:    cfg.registry,
		Repository:  cfg.repository,
		PlainHTTP:   cfg.plainHTTP,
		InsecureTLS: cfg.insecureTLS,
		Username:    cfg.username,
		Password:    envValue(cfg.passwordEnv),
		Token:       envValue(cfg.tokenEnv),
		CAFile:      cfg.caFile,
	})
	if err != nil {
		return fmt.Errorf("push dataset: %w%s", err, authHint(cfg))
	}

	out := pushOutput{
		Reference:       push.Reference,
		Repository:      push.Repository,
		Tag:             push.Tag,
		ManifestDigest:  push.ManifestDigest,
		LocalStore:      cfg.storePath,
		LocalTag:        pkg.LocalTag,
		TotalSize:       pkg.TotalSize,
		DatasetMetadata: hasMetadata,
	}
	if hasMetadata {
		out.MetadataMediaType = sori.MediaTypeDatasetMetadata
	}
	return writePushOutput(os.Stdout, cfg.output, out)
}

func runDatasetInspect(args []string) error {
	var flags registryFlags
	initRegistryFlags(&flags)
	fs := flag.NewFlagSet("dataset inspect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addRegistryFlags(fs, &flags)
	args = reorderFlags(args, registryValueFlags())
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errors.New("usage: sorictl dataset inspect [flags] REGISTRY/REPOSITORY:TAG")
	}
	if err := validateOutput(flags.output); err != nil {
		return err
	}
	configureLibraryLogging(flags.output)
	ref, err := parseImageRef(rest[0])
	if err != nil {
		return err
	}

	out, err := inspectRemote(context.Background(), ref, flags)
	if err != nil {
		return err
	}
	return writeInspectOutput(os.Stdout, flags.output, out)
}

func runDatasetFetch(args []string) error {
	var flags registryFlags
	var overwrite bool
	var skipIfCurrent bool
	initRegistryFlags(&flags)
	fs := flag.NewFlagSet("dataset fetch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addRegistryFlags(fs, &flags)
	fs.BoolVar(&overwrite, "overwrite", false, "replace DEST via atomic overwrite when it already exists")
	fs.BoolVar(&skipIfCurrent, "skip-if-current", false, "skip download when DEST already has the requested manifest digest")
	args = reorderFlags(args, registryValueFlags())
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return errors.New("usage: sorictl dataset fetch [flags] REGISTRY/REPOSITORY:TAG DEST")
	}
	if err := validateOutput(flags.output); err != nil {
		return err
	}
	configureLibraryLogging(flags.output)
	ref, err := parseImageRef(rest[0])
	if err != nil {
		return err
	}
	dest := rest[1]
	if skipIfCurrent && !overwrite {
		return errors.New("--skip-if-current requires --overwrite")
	}

	client := sori.NewClient()
	vi, err := client.FetchVolumeFromRemote(context.Background(), dest, remoteTarget(ref, flags), ref.tag, sori.FetchOptions{
		AtomicOverwrite: overwrite,
		SkipIfCurrent:   skipIfCurrent,
	})
	if err != nil {
		return fmt.Errorf("fetch dataset: %w", err)
	}
	out := fetchOutput{
		Reference:   rest[0],
		Destination: dest,
		VolumeRef:   vi.VolumeRef,
		Skipped:     vi.Skipped,
	}
	metaPath := filepath.Join(dest, ".sori", "dataset-metadata.json")
	if _, err := os.Stat(metaPath); err == nil {
		out.DatasetMetadata = metaPath
	}
	return writeFetchOutput(os.Stdout, flags.output, out)
}

func parsePushConfig(args []string) (pushConfig, error) {
	// #nosec G101 -- these are environment variable names, not embedded credentials.
	cfg := pushConfig{
		storePath:       filepath.Join(os.TempDir(), defaultStoreName),
		kind:            "dataset",
		passwordEnv:     "SORI_REGISTRY_PASSWORD",
		tokenEnv:        "SORI_REGISTRY_TOKEN",
		output:          outputText,
		requireMetadata: true,
		progress:        true,
	}
	fs := flag.NewFlagSet("dataset push", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cfg.ref, "ref", "", "full artifact reference, e.g. ghcr.io/OWNER/references:grch38-bwa-v1")
	fs.StringVar(&cfg.storePath, "store", cfg.storePath, "local OCI store path")
	fs.StringVar(&cfg.registry, "registry", "", "OCI registry host, e.g. ghcr.io or harbor.example.com")
	fs.StringVar(&cfg.repository, "repository", "", "remote repository, e.g. owner/references")
	fs.StringVar(&cfg.tag, "tag", "", "artifact tag")
	fs.StringVar(&cfg.name, "name", "", "dataset machine name")
	fs.StringVar(&cfg.version, "version", "", "dataset version")
	fs.StringVar(&cfg.displayName, "display-name", "", "human-readable display name")
	fs.StringVar(&cfg.description, "description", "", "human-readable description")
	fs.StringVar(&cfg.kind, "kind", cfg.kind, "dataset kind, e.g. reference_genome, annotation, index")
	fs.StringVar(&cfg.sourceURI, "source-uri", "", "upstream source URI recorded in generated metadata")
	fs.StringVar(&cfg.metadataPath, "metadata", "", "dataset-metadata.json path")
	fs.StringVar(&cfg.username, "username", os.Getenv("SORI_REGISTRY_USERNAME"), "registry username")
	fs.StringVar(&cfg.passwordEnv, "password-env", cfg.passwordEnv, "environment variable containing registry password")
	fs.StringVar(&cfg.tokenEnv, "token-env", cfg.tokenEnv, "environment variable containing registry token")
	fs.StringVar(&cfg.output, "output", cfg.output, "output format: text or json")
	fs.BoolVar(&cfg.plainHTTP, "plain-http", false, "use HTTP instead of HTTPS")
	fs.BoolVar(&cfg.insecureTLS, "insecure-tls", false, "skip TLS certificate verification")
	fs.StringVar(&cfg.caFile, "ca-file", "", "custom CA bundle path")
	fs.BoolVar(&cfg.requireMetadata, "require-metadata", true, "fail when metadata cannot be attached")
	fs.BoolVar(&cfg.progress, "progress", true, "print package progress to stderr")
	args = reorderFlags(args, pushValueFlags())
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return cfg, errors.New("usage: sorictl dataset push [flags] SOURCE_DIR")
	}
	cfg.sourceDir = rest[0]
	if cfg.ref != "" {
		ref, err := parseImageRef(cfg.ref)
		if err != nil {
			return cfg, err
		}
		cfg.registry = firstNonEmpty(cfg.registry, ref.registry)
		cfg.repository = firstNonEmpty(cfg.repository, ref.repository)
		cfg.tag = firstNonEmpty(cfg.tag, ref.tag)
	}
	if err := validateSourceDir(cfg.sourceDir); err != nil {
		return cfg, err
	}
	if strings.TrimSpace(cfg.registry) == "" {
		return cfg, errors.New("registry is required; pass --ref REGISTRY/REPOSITORY:TAG or --registry")
	}
	if strings.TrimSpace(cfg.repository) == "" {
		return cfg, errors.New("repository is required; pass --ref REGISTRY/REPOSITORY:TAG or --repository")
	}
	if strings.TrimSpace(cfg.tag) == "" {
		return cfg, errors.New("tag is required; pass --ref REGISTRY/REPOSITORY:TAG or --tag")
	}
	switch cfg.output {
	case outputText, outputJSON:
	default:
		return cfg, validateOutput(cfg.output)
	}
	if strings.TrimSpace(cfg.name) == "" {
		cfg.name = cfg.tag
	}
	return cfg, nil
}

func initRegistryFlags(flags *registryFlags) {
	flags.username = os.Getenv("SORI_REGISTRY_USERNAME")
	flags.passwordEnv = "SORI_REGISTRY_PASSWORD"
	flags.tokenEnv = "SORI_REGISTRY_TOKEN"
	flags.output = outputText
}

func addRegistryFlags(fs *flag.FlagSet, flags *registryFlags) {
	fs.StringVar(&flags.username, "username", flags.username, "registry username")
	fs.StringVar(&flags.passwordEnv, "password-env", flags.passwordEnv, "environment variable containing registry password")
	fs.StringVar(&flags.tokenEnv, "token-env", flags.tokenEnv, "environment variable containing registry token")
	fs.StringVar(&flags.output, "output", flags.output, "output format: text or json")
	fs.BoolVar(&flags.plainHTTP, "plain-http", false, "use HTTP instead of HTTPS")
	fs.BoolVar(&flags.insecureTLS, "insecure-tls", false, "skip TLS certificate verification")
	fs.StringVar(&flags.caFile, "ca-file", "", "custom CA bundle path")
}

func validateOutput(output string) error {
	switch output {
	case outputText, outputJSON:
		return nil
	default:
		return errors.New("--output must be text or json")
	}
}

func configureLibraryLogging(output string) {
	if output == outputJSON {
		sori.Log.SetOutput(io.Discard)
		return
	}
	sori.Log.SetOutput(os.Stderr)
}

func validateSourceDir(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("source directory is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("source directory does not exist: %s", path)
		}
		return fmt.Errorf("check source directory %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source path must be a directory: %s", path)
	}
	return nil
}

func inspectRemote(ctx context.Context, ref imageRef, flags registryFlags) (inspectOutput, error) {
	repo, err := registryutil.NewRepository(ref.registry+"/"+ref.repository, registryutil.RemoteConfig{
		PlainHTTP:   flags.plainHTTP,
		InsecureTLS: flags.insecureTLS,
		Username:    flags.username,
		Password:    envValue(flags.passwordEnv),
		Token:       envValue(flags.tokenEnv),
		CAFile:      flags.caFile,
	})
	if err != nil {
		return inspectOutput{}, err
	}

	desc, err := repo.Resolve(ctx, ref.tag)
	if err != nil {
		return inspectOutput{}, fmt.Errorf("resolve %s: %w", ref.String(), err)
	}
	rc, err := repo.Fetch(ctx, desc)
	if err != nil {
		return inspectOutput{}, fmt.Errorf("fetch manifest: %w", err)
	}
	defer rc.Close()

	var manifest ocispec.Manifest
	if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
		return inspectOutput{}, fmt.Errorf("decode manifest: %w", err)
	}

	out := inspectOutput{
		Reference:       ref.String(),
		ManifestDigest:  desc.Digest.String(),
		ConfigMediaType: manifest.Config.MediaType,
		ArtifactFormat:  artifactFormat(manifest.Config.MediaType),
		LayerCount:      len(manifest.Layers),
	}
	for _, layer := range manifest.Layers {
		if err := inspectLayer(ctx, repo, layer, &out); err != nil {
			return inspectOutput{}, err
		}
	}
	return out, nil
}

func inspectLayer(ctx context.Context, repo interface {
	Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error)
}, layer ocispec.Descriptor, out *inspectOutput) error {
	switch layer.MediaType {
	case sori.MediaTypeChunkIndex:
		idx, err := fetchChunkIndex(ctx, repo, layer)
		if err != nil {
			return err
		}
		applyChunkIndexSummary(out, idx)
	case sori.MediaTypeDatasetMetadata:
		data, err := fetchBlob(ctx, repo, layer)
		if err != nil {
			return fmt.Errorf("fetch dataset metadata: %w", err)
		}
		applyDatasetMetadata(out, layer.MediaType, data)
	}
	return nil
}

func applyChunkIndexSummary(out *inspectOutput, idx sori.ChunkIndex) {
	out.FileCount = len(idx.Files)
	out.ChunkSize = idx.ChunkSize
	for _, file := range idx.Files {
		out.TotalSize += file.Size
		out.ChunkCount += len(file.Chunks)
	}
}

func applyDatasetMetadata(out *inspectOutput, mediaType string, data []byte) {
	out.DatasetMetadata = true
	out.MetadataMediaType = mediaType

	var meta sori.DatasetMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		out.MetadataError = fmt.Sprintf("decode dataset metadata: %v", err)
		return
	}
	out.Metadata = &meta
	if err := sori.ValidateDatasetMetadata(&meta); err != nil {
		out.MetadataError = err.Error()
		return
	}
	out.MetadataValid = true
}

func fetchChunkIndex(ctx context.Context, repo interface {
	Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error)
}, desc ocispec.Descriptor) (sori.ChunkIndex, error) {
	data, err := fetchBlob(ctx, repo, desc)
	if err != nil {
		return sori.ChunkIndex{}, fmt.Errorf("fetch chunk index: %w", err)
	}
	var idx sori.ChunkIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return sori.ChunkIndex{}, fmt.Errorf("decode chunk index: %w", err)
	}
	return idx, nil
}

func fetchBlob(ctx context.Context, repo interface {
	Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error)
}, desc ocispec.Descriptor) ([]byte, error) {
	rc, err := repo.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func loadOrBuildMetadata(cfg pushConfig) ([]byte, bool, error) {
	if cfg.metadataPath != "" {
		data, err := os.ReadFile(cfg.metadataPath)
		if err != nil {
			return nil, false, fmt.Errorf("read metadata: %w", err)
		}
		if err := sori.ValidateDatasetMetadataJSON(data); err != nil {
			return nil, false, err
		}
		return data, true, nil
	}
	if strings.TrimSpace(cfg.displayName) == "" && strings.TrimSpace(cfg.description) == "" {
		return nil, false, nil
	}
	meta := defaultMetadata(cfg)
	if err := sori.ValidateDatasetMetadata(&meta); err != nil {
		return nil, false, err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("encode generated metadata: %w", err)
	}
	return append(data, '\n'), true, nil
}

func defaultMetadata(cfg pushConfig) sori.DatasetMetadata {
	return sori.DatasetMetadata{
		SchemaVersion:    sori.DatasetMetadataSchemaVersion,
		Kind:             cfg.kind,
		DisplayName:      firstNonEmpty(cfg.displayName, cfg.name, cfg.tag),
		Description:      cfg.description,
		SizeBytes:        0,
		Source:           cfg.sourceURI,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		ValidationStatus: "unverified",
		ArtifactRef:      remoteRef(cfg.registry, cfg.repository, cfg.tag),
	}
}

func runMetadataInit(args []string) error {
	var cfg pushConfig
	var organism string
	var taxonomyID string
	var referenceName string
	var referenceVersion string
	var referenceAliases string
	var dataTypes string
	var fileFormats string
	var tools string
	var tags string
	var license string
	var createdBy string
	var validationStatus string
	cfg.kind = "reference_genome"
	validationStatus = "unverified"
	fs := flag.NewFlagSet("metadata init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cfg.ref, "ref", "", "full artifact reference, e.g. ghcr.io/OWNER/references:grch38-bwa-v1")
	fs.StringVar(&cfg.registry, "registry", "", "OCI registry host")
	fs.StringVar(&cfg.repository, "repository", "", "remote repository")
	fs.StringVar(&cfg.tag, "tag", "", "artifact tag")
	fs.StringVar(&cfg.name, "name", "example-reference", "dataset machine name")
	fs.StringVar(&cfg.displayName, "display-name", "Example Reference", "human-readable display name")
	fs.StringVar(&cfg.description, "description", "Describe what this dataset contains and how it should be used.", "human-readable description")
	fs.StringVar(&cfg.kind, "kind", cfg.kind, "dataset kind")
	fs.StringVar(&cfg.sourceURI, "source-uri", "", "upstream source URI")
	fs.StringVar(&organism, "organism", "", "organism scientific name")
	fs.StringVar(&taxonomyID, "taxonomy-id", "", "NCBI taxonomy ID")
	fs.StringVar(&referenceName, "reference", "", "reference build name")
	fs.StringVar(&referenceVersion, "reference-version", "", "reference build version")
	fs.StringVar(&referenceAliases, "reference-alias", "", "comma-separated reference aliases")
	fs.StringVar(&dataTypes, "data-type", "reference_genome", "comma-separated data types")
	fs.StringVar(&fileFormats, "format", "", "comma-separated file formats")
	fs.StringVar(&tools, "tool", "", "comma-separated compatible tools")
	fs.StringVar(&tags, "tag-label", "", "comma-separated metadata tags")
	fs.StringVar(&license, "license", "", "dataset license")
	fs.StringVar(&createdBy, "created-by", "", "metadata author or producing system")
	fs.StringVar(&validationStatus, "validation-status", validationStatus, "validation status, e.g. unverified or validated")
	args = reorderFlags(args, metadataValueFlags())
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.ref != "" {
		ref, err := parseImageRef(cfg.ref)
		if err != nil {
			return err
		}
		cfg.registry = firstNonEmpty(cfg.registry, ref.registry)
		cfg.repository = firstNonEmpty(cfg.repository, ref.repository)
		cfg.tag = firstNonEmpty(cfg.tag, ref.tag)
	}

	meta := defaultMetadata(cfg)
	meta.Organism = sori.Organism{Name: organism, TaxonomyID: taxonomyID}
	meta.Reference = sori.DatasetReference{Name: referenceName, Version: referenceVersion, Aliases: splitCSV(referenceAliases)}
	meta.DataTypes = splitCSV(dataTypes)
	meta.FileFormats = splitCSV(fileFormats)
	meta.CompatibleTools = splitCSV(tools)
	meta.Tags = splitCSV(tags)
	meta.License = license
	meta.CreatedBy = createdBy
	meta.ValidationStatus = validationStatus
	return writeJSON(os.Stdout, meta)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

type textWriter struct {
	w   io.Writer
	err error
}

func (tw *textWriter) printf(format string, args ...any) {
	if tw.err != nil {
		return
	}
	_, tw.err = fmt.Fprintf(tw.w, format, args...)
}

func writePushOutput(w io.Writer, format string, out pushOutput) error {
	if format == outputJSON {
		return writeJSON(w, out)
	}
	tw := &textWriter{w: w}
	tw.printf("Pushed dataset artifact\n")
	tw.printf("  reference: %s\n", out.Reference)
	tw.printf("  digest:    %s\n", out.ManifestDigest)
	tw.printf("  size:      %d bytes\n", out.TotalSize)
	if out.DatasetMetadata {
		tw.printf("  metadata:  attached (%s)\n", out.MetadataMediaType)
	} else {
		tw.printf("  metadata:  not attached\n")
	}
	return tw.err
}

func writeInspectOutput(w io.Writer, format string, out inspectOutput) error {
	if format == outputJSON {
		return writeJSON(w, out)
	}
	return writeInspectText(w, out)
}

func writeInspectText(w io.Writer, out inspectOutput) error {
	tw := &textWriter{w: w}
	tw.printf("Dataset artifact\n")
	tw.printf("  reference: %s\n", out.Reference)
	tw.printf("  digest:    %s\n", out.ManifestDigest)
	tw.printf("  format:    %s\n", firstNonEmpty(out.ArtifactFormat, out.ConfigMediaType))
	tw.printf("  layers:    %d\n", out.LayerCount)
	if out.FileCount > 0 {
		tw.printf("  files:     %d\n", out.FileCount)
	}
	if out.ChunkCount > 0 {
		tw.printf("  chunks:    %d", out.ChunkCount)
		if out.ChunkSize > 0 {
			tw.printf(" (%d byte chunk size)", out.ChunkSize)
		}
		tw.printf("\n")
	}
	if out.TotalSize > 0 {
		tw.printf("  size:      %d bytes\n", out.TotalSize)
	}
	if !out.DatasetMetadata || out.Metadata == nil {
		if out.MetadataError != "" {
			tw.printf("  metadata:  invalid (%s)\n", out.MetadataError)
			return tw.err
		}
		tw.printf("  metadata:  not attached\n")
		return tw.err
	}
	if out.MetadataValid {
		tw.printf("  metadata:  attached (%s)\n", out.MetadataMediaType)
	} else {
		tw.printf("  metadata:  attached but invalid (%s)\n", out.MetadataError)
	}
	tw.printf("\n")
	tw.printf("Dataset metadata\n")
	tw.printf("  name:        %s\n", out.Metadata.DisplayName)
	tw.printf("  kind:        %s\n", out.Metadata.Kind)
	tw.printf("  description: %s\n", out.Metadata.Description)
	if out.Metadata.Organism.Name != "" {
		tw.printf("  organism:    %s", out.Metadata.Organism.Name)
		if out.Metadata.Organism.TaxonomyID != "" {
			tw.printf(" (%s)", out.Metadata.Organism.TaxonomyID)
		}
		tw.printf("\n")
	}
	if out.Metadata.Reference.Name != "" {
		tw.printf("  reference:   %s", out.Metadata.Reference.Name)
		if out.Metadata.Reference.Version != "" {
			tw.printf(" %s", out.Metadata.Reference.Version)
		}
		tw.printf("\n")
	}
	if len(out.Metadata.CompatibleTools) > 0 {
		tw.printf("  tools:       %s\n", strings.Join(out.Metadata.CompatibleTools, ", "))
	}
	if len(out.Metadata.Tags) > 0 {
		tw.printf("  tags:        %s\n", strings.Join(out.Metadata.Tags, ", "))
	}
	return tw.err
}

func writeFetchOutput(w io.Writer, format string, out fetchOutput) error {
	if format == outputJSON {
		return writeJSON(w, out)
	}
	tw := &textWriter{w: w}
	if out.Skipped {
		tw.printf("Dataset already current\n")
	} else {
		tw.printf("Fetched dataset artifact\n")
	}
	tw.printf("  reference:   %s\n", out.Reference)
	tw.printf("  destination: %s\n", out.Destination)
	tw.printf("  digest:      %s\n", out.VolumeRef)
	if out.DatasetMetadata != "" {
		tw.printf("  metadata:    %s\n", out.DatasetMetadata)
	}
	return tw.err
}

func progressPrinter(enabled bool) sori.ProgressFunc {
	if !enabled {
		return nil
	}
	var uploaded int
	var skipped int
	return func(cp sori.ChunkProgress) {
		switch cp.Event {
		case "ChunkUploaded":
			uploaded++
			fmt.Fprintf(os.Stderr, "uploaded chunk %d: %s (%d bytes)\n", uploaded, cp.File, cp.Bytes)
		case "ChunkSkipped":
			skipped++
			fmt.Fprintf(os.Stderr, "reused chunk %d: %s\n", skipped, cp.File)
		case "ArtifactDone":
			fmt.Fprintf(os.Stderr, "packaging complete: %d uploaded, %d reused\n", uploaded, skipped)
		}
	}
}

func authHint(cfg pushConfig) string {
	if cfg.username == "" && envValue(cfg.tokenEnv) == "" && envValue(cfg.passwordEnv) == "" {
		return "\nHint: registry push usually needs credentials. " +
			"Pass --username and --password-env, or set SORI_REGISTRY_USERNAME " +
			"plus SORI_REGISTRY_TOKEN/SORI_REGISTRY_PASSWORD."
	}
	if envValue(cfg.tokenEnv) == "" && envValue(cfg.passwordEnv) == "" {
		return fmt.Sprintf(
			"\nHint: %s and %s are empty. Put your registry token/password in one of them, "+
				"or pass --password-env with the variable name you use.",
			cfg.tokenEnv,
			cfg.passwordEnv,
		)
	}
	return ""
}

func envValue(name string) string {
	if name == "" {
		return ""
	}
	return os.Getenv(name)
}

func remoteRef(registry, repository, tag string) string {
	if registry == "" || repository == "" || tag == "" {
		return ""
	}
	return strings.TrimRight(registry, "/") + "/" + strings.TrimLeft(repository, "/") + ":" + tag
}

func remoteTarget(ref imageRef, flags registryFlags) sori.RemoteTarget {
	return sori.RemoteTarget{
		Registry:    ref.registry,
		Repository:  ref.repository,
		PlainHTTP:   flags.plainHTTP,
		InsecureTLS: flags.insecureTLS,
		Username:    flags.username,
		Password:    envValue(flags.passwordEnv),
		Token:       envValue(flags.tokenEnv),
		CAFile:      flags.caFile,
	}
}

func artifactFormat(configMediaType string) string {
	switch configMediaType {
	case sori.MediaTypeChunkedConfig:
		return "chunked-cas"
	default:
		return configMediaType
	}
}

func parseImageRef(raw string) (imageRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return imageRef{}, errors.New("reference is required")
	}
	firstSlash := strings.Index(raw, "/")
	lastColon := strings.LastIndex(raw, ":")
	if firstSlash <= 0 || lastColon <= firstSlash+1 || lastColon == len(raw)-1 {
		return imageRef{}, fmt.Errorf("reference must be REGISTRY/REPOSITORY:TAG, got %q; example: ghcr.io/OWNER/references:grch38-bwa-v1", raw)
	}
	return imageRef{
		registry:   raw[:firstSlash],
		repository: raw[firstSlash+1 : lastColon],
		tag:        raw[lastColon+1:],
	}, nil
}

func (r imageRef) String() string {
	return r.registry + "/" + r.repository + ":" + r.tag
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func reorderFlags(args []string, valueFlags map[string]struct{}) []string {
	var flags []string
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if eq := strings.Index(name, "="); eq >= 0 {
			name = name[:eq]
		}
		if _, needsValue := valueFlags[name]; needsValue && !strings.Contains(arg, "=") && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

func registryValueFlags() map[string]struct{} {
	return map[string]struct{}{
		"username":     {},
		"password-env": {},
		"token-env":    {},
		"output":       {},
		"ca-file":      {},
	}
}

func pushValueFlags() map[string]struct{} {
	flags := registryValueFlags()
	for _, name := range []string{
		"ref", "store", "registry", "repository", "tag", "name", "version",
		"display-name", "description", "kind", "source-uri", "metadata",
	} {
		flags[name] = struct{}{}
	}
	return flags
}

func metadataValueFlags() map[string]struct{} {
	return map[string]struct{}{
		"ref":               {},
		"registry":          {},
		"repository":        {},
		"tag":               {},
		"name":              {},
		"display-name":      {},
		"description":       {},
		"kind":              {},
		"source-uri":        {},
		"organism":          {},
		"taxonomy-id":       {},
		"reference":         {},
		"reference-version": {},
		"reference-alias":   {},
		"data-type":         {},
		"format":            {},
		"tool":              {},
		"tag-label":         {},
		"license":           {},
		"created-by":        {},
		"validation-status": {},
	}
}

func printUsage(w *os.File) {
	_, _ = fmt.Fprintln(w, `sorictl packages reference datasets as OCI artifacts.

Usage:
  sorictl metadata init [flags]
  sorictl dataset push [flags] SOURCE_DIR
  sorictl dataset inspect [flags] REGISTRY/REPOSITORY:TAG
  sorictl dataset fetch [flags] REGISTRY/REPOSITORY:TAG DEST

Examples:
  sorictl metadata init --ref ghcr.io/OWNER/references:grch38-bwa-v1 --name grch38-bwa > dataset-metadata.json
  sorictl dataset push ./grch38-bwa --ref ghcr.io/OWNER/references:grch38-bwa-v1 --metadata dataset-metadata.json --username USER --password-env GHCR_TOKEN
  sorictl dataset inspect ghcr.io/OWNER/references:grch38-bwa-v1 --username USER --password-env GHCR_TOKEN
  sorictl dataset fetch harbor.example.com/bio/refdata:refs-v1 ./refs --username USER --password-env HARBOR_PASSWORD --overwrite`)
}

func printDatasetUsage(w *os.File) {
	_, _ = fmt.Fprintln(w, `Usage:
  sorictl dataset push SOURCE_DIR --ref REGISTRY/REPOSITORY:TAG --metadata dataset-metadata.json
  sorictl dataset inspect REGISTRY/REPOSITORY:TAG
  sorictl dataset fetch REGISTRY/REPOSITORY:TAG DEST

Beginner flow:
  1. Create dataset-metadata.json with "sorictl metadata init".
  2. Push a local data directory with "sorictl dataset push".
  3. Check what was uploaded with "sorictl dataset inspect".
  4. Fetch it back into a new directory with "sorictl dataset fetch".

Auth:
  Set SORI_REGISTRY_USERNAME and SORI_REGISTRY_TOKEN, or pass --username and --password-env.`)
}

func printMetadataUsage(w *os.File) {
	_, _ = fmt.Fprintln(w, `Usage:
  sorictl metadata init --ref REGISTRY/REPOSITORY:TAG --name NAME --display-name DISPLAY --description TEXT

Example:
  sorictl metadata init \
    --ref ghcr.io/OWNER/references:grch38-bwa-v1 \
    --name grch38-bwa \
    --display-name "GRCh38 BWA Index" \
    --description "Human GRCh38 BWA index." \
    --format bwa-index \
    --tool bwa,bwa-mem2 \
    > dataset-metadata.json`)
}
