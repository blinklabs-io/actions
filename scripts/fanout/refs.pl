#!/usr/bin/env perl
# Rewrite a product's version references in one documentation or example file.
# Rules rewrite image refs, helm tags, prose pins, <product>_version, the
# image.tag table row and arch-suffixed tags.
# Usage: refs.pl PRODUCT VERSION FILE   (VERSION is X.Y.Z, no "v")
# Exit 0 when the file changed, 1 when it did not, 255 on any failure. Never
# lowers a version.
use strict;
use warnings;

# A bare die exits with errno, which can be 1 and read as "unchanged".
BEGIN { $SIG{__DIE__} = sub { print STDERR @_; exit 255 } }

my ($product, $new, $file) = @ARGV;
die "usage: refs.pl PRODUCT VERSION FILE\n" unless defined $file;

my $q   = quotemeta($product);
my $ver = qr/[0-9]+\.[0-9]+\.[0-9]+/;
my $img = qr{(?:ghcr\.io/blinklabs-io|blinklabs)/$q};
my $end = qr/(?![\w\-]|\.[0-9])/;

sub newer {
    my @a = split /\./, shift;
    my @b = split /\./, shift;
    for my $i (0 .. 2) {
        return 1 if $a[$i] > $b[$i];
        return 0 if $a[$i] < $b[$i];
    }
    return 0;
}

sub rep {
    my ($pre, $vp, $old, $post) = @_;
    return $pre . $vp . (newer($new, $old) ? $new : $old) . $post;
}

open my $in, '<', $file or die "open $file: $!\n";
local $/;
my $text = <$in>;
close $in;
my $orig = $text;

# 1. Image references: ghcr.io/blinklabs-io/<product>:<version>
$text =~ s{($img:)(v?)($ver)$end}{rep($1, $2, $3, '')}ge;

# 2. Helm values: rewrite a "tag:" line only when it is a sibling of the
#    nearest preceding "repository:" line and that line names this product's
#    image. The state ends when a line is indented less than the repository
#    line (its mapping closed) or another repository line appears.
my @lines = split /^/, $text, -1;
my $repo_indent;
for my $l (@lines) {
    if ($l =~ m{^(\s*)repository:\s*["']?(\S+?)["']?(?:[ \t]+\#.*)?\s*$}) {
        my ($indent, $name) = (length($1), $2);
        $repo_indent = ($name =~ m{^$img$}) ? $indent : undef;
    } elsif (defined $repo_indent) {
        next if $l =~ m{^\s*(?:\#.*)?$};
        my ($lead) = $l =~ m{^([ \t]*)};
        if (length($lead) < $repo_indent) {
            undef $repo_indent;
        } elsif (length($lead) == $repo_indent) {
            $l =~ s{^(\s*tag:\s*["']?)(v?)($ver)(["']?[ \t]*(?:\#.*)?)(\r?\n?)$}{rep($1, $2, $3, $4) . $5}e;
        }
    }
}
$text = join '', @lines;

# 3. Prose: <Product> `<version>`
$text =~ s{(\b$q\s+`)(v?)($ver)(`)}{rep($1, $2, $3, $4)}gie;

# 4. A "<product>_version" variable: a Markdown table cell or a YAML/ini
#    assignment. Only the version token is replaced.
(my $var = $product) =~ s/-/_/g;
my $qv = quotemeta($var) . '_version';
$text =~ s{((?<![\w])$qv\b`?[ \t]*[|:=][ \t]*[`'"| ]*)(v?)($ver)$end}{rep($1, $2, $3, '')}gie;

# 5. A Markdown table row for `image.tag`: the last backticked version on the
#    row is the default value.
@lines = split /^/, $text, -1;
for my $l (@lines) {
    next unless $l =~ m{^\s*\|\s*`image\.tag`\s*\|};
    my $last;
    while ($l =~ m{`(v?)($ver)`}g) { $last = [$-[0], $+[0], $1, $2]; }
    next unless $last;
    my ($s, $e, $vp, $old) = @$last;
    substr($l, $s, $e - $s) = '`' . $vp . (newer($new, $old) ? $new : $old) . '`';
}
$text = join '', @lines;

# 6. A backticked architecture-specific tag: `<version>-amd64`, `-arm64` or
#    `-arm64v8`. The architecture suffix keeps this off ordinary versions.
$text =~ s{(`)(v?)($ver)(-(?:amd64|arm64v8|arm64)`)}{rep($1, $2, $3, $4)}ge;

exit 1 if $text eq $orig;
open my $out, '>', $file or die "write $file: $!\n";
print $out $text or die "write $file: $!\n";
close $out or die "write $file: $!\n";
print STDERR "refs: updated $file\n";
exit 0;
