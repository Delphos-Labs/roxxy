# License Compatibility Checker

A tool to check for incompatible licenses in your project's SBOM (Software Bill of Materials).

## Usage

```bash
./check-licenses -sbom <sbom-file.json> -license <your-license>
```

## Options

- `-sbom <file>`: Path to SBOM JSON file (required)
- `-license <license>`: Your project's license (SPDX identifier, e.g., MIT, Apache-2.0, GPL-3.0) (required)
- `-all`: Show all components, not just incompatible ones
- `-verbose`: Show detailed license information

## Examples

Check for incompatible licenses in an SBOM with an MIT-licensed project:

```bash
./check-licenses -sbom sbom.json -license MIT
```

Show all components and their license compatibility:

```bash
./check-licenses -sbom sbom.json -license Apache-2.0 -all -verbose
```

## Supported Licenses

The tool supports compatibility checking for common open-source licenses:

- **Permissive**: MIT, Apache-2.0, BSD-2-Clause, BSD-3-Clause, ISC, MPL-2.0
- **Copyleft**: GPL-2.0-only, GPL-3.0-only, LGPL-2.1-only, LGPL-3.0-only, AGPL-3.0-only

## License Compatibility Rules

- **MIT**: Compatible with almost all licenses
- **Apache-2.0**: Compatible with most licenses (except GPL-2.0-only)
- **GPL-3.0-only**: Only compatible with GPL-3.0, AGPL-3.0, and LGPL-3.0
- **GPL-2.0-only**: Only compatible with GPL-2.0-only and LGPL-2.1
- **LGPL**: Compatible with GPL of the same version and permissive licenses
- **BSD/ISC/MPL**: Compatible with most licenses

## Exit Codes

- `0`: All licenses are compatible (or no incompatible licenses found)
- `1`: Incompatible licenses found

## Notes

- Components without license information are reported but don't cause the check to fail
- The tool uses SPDX license identifiers for matching
- License expressions (e.g., "MIT OR Apache-2.0") are simplified to the first license in OR expressions
