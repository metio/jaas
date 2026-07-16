# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD

# One-shot production build of the documentation site into docs/public/, with
# every Hugo warning surfaced. This is what CI's docs-lint job builds before
# htmltest runs, so a warning you see here is one the gate sees.

hugo --minify --printI18nWarnings --printPathWarnings --printUnusedTemplates --source docs
