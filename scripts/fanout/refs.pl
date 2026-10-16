#!/usr/bin/env perl
# Rewrite a product's version references in one documentation or example file.
# Usage: refs.pl PRODUCT VERSION FILE   (VERSION is X.Y.Z, no "v")
# Exit 0 when the file changed, 1 when it did not. Never lowers a version.
use strict;
use warnings;

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

# 2. Helm values: rewrite a "tag:" line only when the nearest preceding
#    "repository:" line names this product's image.
my @lines = split /^/, $text, -1;
my $current_is_product = 0;
for my $l (@lines) {
    if ($l =~ m{^\s*repository:\s*["']?(\S+?)["']?(?:[ \t]+\#.*)?\s*$}) {
        $current_is_product = ($1 =~ m{^$img$}) ? 1 : 0;
    } elsif ($current_is_product) {
        $l =~ s{^(\s*tag:\s*["']?)(v?)($ver)(["']?[ \t]*(?:\#.*)?)(\r?\n?)$}{rep($1, $2, $3, $4) . $5}e;
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

exit 1 if $text eq $orig;
open my $out, '>', $file or die "write $file: $!\n";
print $out $text;
close $out;
print STDERR "refs: updated $file\n";
exit 0;
