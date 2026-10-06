mod tokens {
    include!(concat!(env!("OUT_DIR"), "/theme_tokens.rs"));
}
fn main() {
    for (k, v) in tokens::LIGHT { println!("light {k} = {v}"); }
    for (f, b) in tokens::FONTS { println!("font {f} {}", b.len()); }
}
