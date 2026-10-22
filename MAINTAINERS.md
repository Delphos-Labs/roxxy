# Roxxy maintainers

Canonical repository: [Delphos-Labs/roxxy](https://github.com/Delphos-Labs/roxxy).

## Intended roles

Faye Salwin ([`@beiriannydd`](https://github.com/beiriannydd)) is the proposed lead maintainer.
The internal maintainer team is Code Wizards (`@Delphos-Labs/code-wizards`).
Code Wizards has Write access. Faye has a pending invitation for Maintain access as an outside collaborator.
Her lead maintainer role takes effect after she accepts the invitation and confirms the role.

The lead maintainer would guide technical decisions, review contributions, and coordinate releases and security fixes.
The internal team would provide independent review, release continuity, and company coordination.
Repository administrators retain responsibility for access, branch protection, and publication settings.

## Activate ownership

Before public release, administrators must:

1. Confirm Faye's identity and acceptance of the proposed role.
2. Verify Code Wizards' explicit Write access.
3. Verify that Faye accepted her Maintain invitation.
4. Add Faye to the CODEOWNERS catch-all rule after she accepts write-capable access.
5. Validate the listed owners in GitHub and enable required code-owner review on `main`.
6. Confirm an independent maintainer can review each maintainer's own changes.

Faye can be an outside collaborator. Repository access does not require organization membership.
Do not grant organization-wide access solely to satisfy CODEOWNERS.
Membership in the internal team requires organization membership, so use that team for authorized internal maintainers.

## Approval rules

[CODEOWNERS](.github/CODEOWNERS) lists Code Wizards on one catch-all rule.
An eligible Code Wizards member can satisfy GitHub's code-owner approval requirement.
After Faye is added to that rule, GitHub accepts approval from either the team or Faye. It does not require both.
An owner must have write access for ownership to work. The team must also have explicit write access.
Protection must require code-owner review for this file to enforce approval.
Validate the rule after each ownership change. Faye's code-owner role remains pending invitation acceptance.

Require independent review even when a maintainer writes the change.
If the author is Faye, another eligible maintainer must review it.
If no independent reviewer is available, wait for one rather than bypassing review.

## Support and releases

Use [SUPPORT.md](SUPPORT.md) for ordinary requests and [SECURITY.md](SECURITY.md) for vulnerability reporting.
Maintainer status does not promise a response time, support contract, or security maintenance period.
Approve the exact release commit before publication. Publish only through the reviewed, trusted release process.
