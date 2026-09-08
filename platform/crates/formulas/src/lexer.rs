//! Tokenizer for the formula DSL.

#[derive(Debug, Clone, PartialEq)]
pub enum Token {
    Number(f64),
    Str(String),
    Ident(String),
    Plus,
    Minus,
    Star,
    Slash,
    Percent,
    LParen,
    RParen,
    Comma,
    Eq,    // ==
    Neq,   // !=
    Lt,
    Lte,
    Gt,
    Gte,
    And,   // 'and' or &&
    Or,    // 'or' or ||
    Not,   // 'not' or !
    Eof,
}

#[derive(Debug, Clone)]
pub struct LexError {
    pub message: String,
}

pub fn lex(input: &str, max_tokens: usize) -> Result<Vec<Token>, LexError> {
    let mut tokens = Vec::new();
    let chars: Vec<char> = input.chars().collect();
    let mut i = 0;
    let n = chars.len();

    while i < n {
        let c = chars[i];
        match c {
            ' ' | '\t' | '\n' | '\r' => {
                i += 1;
            }
            '+' => {
                tokens.push(Token::Plus);
                i += 1;
            }
            '-' => {
                tokens.push(Token::Minus);
                i += 1;
            }
            '*' => {
                tokens.push(Token::Star);
                i += 1;
            }
            '/' => {
                // Comments are not part of the DSL; plain division.
                tokens.push(Token::Slash);
                i += 1;
            }
            '%' => {
                tokens.push(Token::Percent);
                i += 1;
            }
            '(' => {
                tokens.push(Token::LParen);
                i += 1;
            }
            ')' => {
                tokens.push(Token::RParen);
                i += 1;
            }
            ',' => {
                tokens.push(Token::Comma);
                i += 1;
            }
            '&' => {
                if i + 1 < n && chars[i + 1] == '&' {
                    tokens.push(Token::And);
                    i += 2;
                } else {
                    return Err(LexError {
                        message: "single '&' is not an operator; use 'and' or '&&'".into(),
                    });
                }
            }
            '|' => {
                if i + 1 < n && chars[i + 1] == '|' {
                    tokens.push(Token::Or);
                    i += 2;
                } else {
                    return Err(LexError {
                        message: "single '|' is not an operator; use 'or' or '||'".into(),
                    });
                }
            }
            '!' => {
                if i + 1 < n && chars[i + 1] == '=' {
                    tokens.push(Token::Neq);
                    i += 2;
                } else {
                    tokens.push(Token::Not);
                    i += 1;
                }
            }
            '=' => {
                if i + 1 < n && chars[i + 1] == '=' {
                    tokens.push(Token::Eq);
                    i += 2;
                } else {
                    return Err(LexError {
                        message: "single '=' is not an operator; use '=='".into(),
                    });
                }
            }
            '<' => {
                if i + 1 < n && chars[i + 1] == '=' {
                    tokens.push(Token::Lte);
                    i += 2;
                } else {
                    tokens.push(Token::Lt);
                    i += 1;
                }
            }
            '>' => {
                if i + 1 < n && chars[i + 1] == '=' {
                    tokens.push(Token::Gte);
                    i += 2;
                } else {
                    tokens.push(Token::Gt);
                    i += 1;
                }
            }
            '"' | '\'' => {
                let quote = c;
                let start = i + 1;
                let mut j = start;
                while j < n && chars[j] != quote {
                    j += 1;
                }
                if j >= n {
                    return Err(LexError { message: "unterminated string literal".into() });
                }
                tokens.push(Token::Str(chars[start..j].iter().collect()));
                i = j + 1;
            }
            '0'..='9' => {
                let start = i;
                while i < n && (chars[i].is_ascii_digit() || chars[i] == '.') {
                    i += 1;
                }
                let text: String = chars[start..i].iter().collect();
                let v: f64 = text
                    .parse()
                    .map_err(|_| LexError { message: format!("invalid number `{}`", text) })?;
                tokens.push(Token::Number(v));
            }
            _ if c.is_alphabetic() || c == '_' => {
                let start = i;
                while i < n && (chars[i].is_alphanumeric() || chars[i] == '_' || chars[i] == '.') {
                    i += 1;
                }
                let word: String = chars[start..i].iter().collect();
                // Idents may contain dots for path variables (event.payload.points).
                let lower = word.to_lowercase();
                match lower.as_str() {
                    "and" | "true" | "false" => match lower.as_str() {
                        "and" => tokens.push(Token::And),
                        "true" => tokens.push(Token::Number(1.0)),
                        _ => tokens.push(Token::Number(0.0)),
                    },
                    "or" => tokens.push(Token::Or),
                    "not" => tokens.push(Token::Not),
                    _ => tokens.push(Token::Ident(word)),
                }
            }
            _ => {
                return Err(LexError { message: format!("unexpected character `{}`", c) });
            }
        }
        if tokens.len() > max_tokens {
            return Err(LexError {
                message: format!("expression exceeds {} tokens", max_tokens),
            });
        }
    }

    tokens.push(Token::Eof);
    Ok(tokens)
}
