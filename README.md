# Git LFS Server

Bring Your Own Forge (BYOF) Git LFS server with affordable storage.

## Price comparison

Assuming 1 TiB is uploaded and stored for a full month, and 3 TiB is downloaded in the same month:

| Provider | Storage | Egress | Yearly total | Savings vs. GitLab LFS |
| --- | ---: | ---: | ---: | ---: |
| GitLab LFS | 109 x 10 GB-month x $5 x 12 = $6,540.00 | Included | $6,540.00 | Baseline |
| GitHub LFS | 1,014 GiB x $0.07 x 12 = $851.76 | 36,744 GiB x $0.0875 = $3,215.10 | $4,066.86 | $2,473.14 (37.81%) |
| DigitalOcean Spaces | ($5 + 774 GiB x $0.02) x 12 = $245.76 | 24,576 GiB x $0.01 = $245.76 | $491.52 | $6,048.48 (92.48%) |
| Cloudflare R2 Standard | 1,090 GB-month x $0.015 x 12 = $196.20 | Included | $196.20 | $6,343.80 (97.00%) |
| Backblaze B2 | 1 TB x $6.95 x 12 = $83.40 | Free up to 3x storage | $83.40 | $6,456.60 (98.72%) |

## License

This project is under the MIT License. See the [LICENSE](LICENSE) file for the full license text.
