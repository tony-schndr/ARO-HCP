# Image Updater

Automatically fetches the latest image digests from container registries and updates ARO-HCP configuration files.

## Managed Images

| Image Name | Image Reference |
|------------|-----------------|
| maestro | quay.io/redhat-user-workloads/maestro-rhtap-tenant/maestro/maestro |
| hypershift | quay.io/acm-d/rhtap-hypershift-operator |
| pko-package | quay.io/package-operator/package-operator-package |
| pko-manager | quay.io/package-operator/package-operator-manager |
| pko-remote-phase-manager | quay.io/package-operator/remote-phase-manager |
| arohcpfrontend | arohcpsvcdev.azurecr.io/arohcpfrontend |
| arohcpbackend | arohcpsvcdev.azurecr.io/arohcpbackend |

## Usage

### Manual Updates

```bash
# Update all images
make update

# Preview changes without modifying files
./image-updater update --config config.yaml --dry-run

# Update with custom config
./image-updater update --config config.yaml
```

### Automated PR Creation

The `autobump` command integrates with prow's generic-autobumper to automatically create pull requests:

```bash
# Using make
make autobump

# Or run directly
./image-updater autobump \
  --config config.yaml \
  --bumper-config autobump-config.yaml
```

This will:
1. Fetch the latest image digests
2. Update configuration files
3. Commit the changes
4. Push to a fork
5. Create or update a pull request

See `autobump-config.yaml` for configuration options including GitHub credentials, PR labels, and oncall assignment.

## Configuration

Define images to monitor and target files to update:

```yaml
images:
  maestro:
    source:
      image: quay.io/redhat-user-workloads/maestro-rhtap-tenant/maestro/maestro
      tagPattern: "^[a-f0-9]{40}$"  # Optional regex to filter tags
    targets:
    - jsonPath: clouds.dev.defaults.maestro.image.digest
      filePath: ../../config/config.yaml

  pko-package:
    source:
      image: quay.io/package-operator/package-operator-package
      tagPattern: "^v\\d+\\.\\d+\\.\\d+$"  # Match semver tags
    targets:
    - jsonPath: defaults.pko.imagePackage.digest
      filePath: ../../config/config.yaml
```

## Tag Patterns

Common regex patterns for filtering tags:

- `^[a-f0-9]{40}$` - 40-character commit hashes
- `^latest$` - Only 'latest' tag
- `^v\\d+\\.\\d+\\.\\d+$` - Semantic versions (v1.2.3)
- `^main-.*` - Tags starting with 'main-'

If no pattern is specified, uses the most recently pushed tag.

## Registry Support

- **Quay.io**: Public repositories (no auth required)
- **Azure Container Registry**: Requires `az login` authentication

## Command Options

```
Flags:
      --config string   Path to configuration file (required)
      --dry-run         Preview changes without modifying files
```