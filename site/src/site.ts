/**
 * Links used in more than one place. Kept here so the page cannot disagree with
 * itself about where the repository is.
 */
export const REPO = "https://github.com/thuupx/hive";
export const DOCS = `${REPO}/blob/main/docs`;

export const DOC_LINKS = [
  { href: `${DOCS}/reference.md`, label: "Reference" },
  { href: `${DOCS}/protocol.md`, label: "Protocol" },
  { href: `${DOCS}/security.md`, label: "Security" },
  { href: `${DOCS}/configuration.md`, label: "Configuration" },
  { href: `${DOCS}/failure-semantics.md`, label: "Failure semantics" },
];

/** The one-line install, shown in the hero and the quick start.
 *
 * The script is attached to every release, so this URL always serves the
 * versioned copy rather than whatever is on main. */
export const INSTALL_COMMAND =
  "curl -fsSL https://github.com/thuupx/hive/releases/latest/download/install.sh | sh";
