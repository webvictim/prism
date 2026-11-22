---
name: prism-beams
description: Record this AI session in Teleport Beams and add the recording to a pull request, as policy requires. Use when opening, raising or updating a pull request (gh pr create, gh pr edit), when asked for AI disclosure or evidence of AI involvement, or when asked about Beams session recordings. Applies when inference runs through prism.
---
<!-- prism-managed: written by `prism setup skill`; prism overwrites this file on update. Remove the marker line to keep local edits. -->

# Recording AI work in Teleport Beams with prism

Policy requires every pull request containing AI-generated code to link to a session recorded in Beams. prism sends your inference out through a Teleport beam, so the session is recorded, summarised and classified in Beams as if you were running there, while you, the code and your tools stay on this machine. Each agent session gets a beam of its own, started automatically.

## If a request fails with "prism: request not sent"

prism sends inference only through a beam, and starting one needs the user's own Teleport login. The message says what to do: usually to open a login link (or run `prism login` in a terminal). Show the user the message, including the link, verbatim, and ask them to retry once they've logged in. Don't try to work around it, and don't suggest sending inference without a beam.

## When opening or updating a pull request

1. Check what recorded this work:

   ```bash
   prism beam status
   ```

   It lists the beams running; those marked `*` belong to this repository.

2. If the user has finished with the work, end this repository's beams so their summaries can start (they appear in Beams about 15 minutes after a beam ends):

   ```bash
   prism beam end
   ```

   Skip this if they're carrying on. A beam still in use is listed as such, and is summarised once it ends (or after two hours unused).

3. Generate the section for the PR description, from the repository's directory:

   ```bash
   prism beam link --pr            # this repository's beams in the last 24h
   prism beam link --pr --since 72h
   ```

4. Put the output, verbatim, in the PR body, e.g. `gh pr create --body-file` with the section appended. **Don't edit the beam names, times or links, and never invent them.** If the command reports no beam, tell the user that this work wasn't recorded, instead of writing a disclosure without one.

5. The policy also asks for evidence of human design and review. That is the author's to describe. Don't write it for them; leave a note for the user to add it.

## Notes

- A beam lives at most 11 hours. prism replaces it before it expires, so long work can span several beams, and `prism beam link --pr` lists each one.
- If `prism beam status` says the sessions aren't recorded because this machine hasn't switched yet, the user needs to run `prism setup beams` once. Tell them; don't run it yourself, since it logs them in.
- `prism beam status --json` gives machine-readable status.
